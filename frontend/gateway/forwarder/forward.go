package forwarder

import (
	"context"
	"sync"

	cacheutil "github.com/moby/buildkit/cache/util"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/gateway"
	"github.com/moby/buildkit/frontend/gateway/client"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/errdefs"
	llberrdefs "github.com/moby/buildkit/solver/llbsolver/errdefs"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/attestation"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

func llbBridgeToGatewayClient(ctx context.Context, llbBridge frontend.FrontendLLBBridge, opts map[string]string, inputs map[string]*opspb.Definition, w worker.Infos, sid string, sm *session.Manager) (*bridgeClient, error) {
	bc := &bridgeClient{
		opts:              opts,
		inputs:            inputs,
		FrontendLLBBridge: llbBridge,
		sid:               sid,
		sm:                sm,
		workers:           w,
		workerRefByID:     make(map[string]*worker.WorkerRef),
	}
	bc.buildOpts = bc.loadBuildOpts()
	return bc, nil
}

type bridgeClient struct {
	frontend.FrontendLLBBridge
	mu            sync.Mutex
	opts          map[string]string
	inputs        map[string]*opspb.Definition
	sid           string
	sm            *session.Manager
	refs          []*ref
	workers       worker.Infos
	workerRefByID map[string]*worker.WorkerRef
	buildOpts     client.BuildOpts
	ctrs          []client.Container
}

func (c *bridgeClient) Solve(ctx context.Context, req client.SolveRequest) (*client.Result, error) {
	res, err := c.FrontendLLBBridge.Solve(ctx, frontend.SolveRequest{
		Evaluate:       req.Evaluate,
		Definition:     req.Definition,
		Frontend:       req.Frontend,
		FrontendOpt:    req.FrontendOpt,
		FrontendInputs: req.FrontendInputs,
		CacheImports:   req.CacheImports,
	}, c.sid)
	if err != nil {
		return nil, c.wrapSolveError(err)
	}

	cRes := &client.Result{}
	c.mu.Lock()
	for k, r := range res.Refs {
		rr, err := c.newRef(r, session.NewGroup(c.sid))
		if err != nil {
			return nil, err
		}
		c.refs = append(c.refs, rr)
		cRes.AddRef(k, rr)
	}
	if r := res.Ref; r != nil {
		rr, err := c.newRef(r, session.NewGroup(c.sid))
		if err != nil {
			return nil, err
		}
		c.refs = append(c.refs, rr)
		cRes.SetRef(rr)
	}
	for k, as := range res.Attestations {
		for _, a := range as {
			att, rrs, err := c.newAttestation(a, session.NewGroup(c.sid))
			if err != nil {
				return nil, err
			}
			c.refs = append(c.refs, rrs...)
			cRes.AddAttestation(k, att)
		}
	}
	c.mu.Unlock()
	cRes.Metadata = res.Metadata

	return cRes, nil
}
func (c *bridgeClient) loadBuildOpts() client.BuildOpts {
	wis := c.workers.WorkerInfos()
	workers := make([]client.WorkerInfo, len(wis))
	for i, w := range wis {
		workers[i] = client.WorkerInfo{
			ID:        w.ID,
			Labels:    w.Labels,
			Platforms: w.Platforms,
		}
	}

	return client.BuildOpts{
		Opts:      c.opts,
		SessionID: c.sid,
		Workers:   workers,
		Product:   apicaps.ExportedProduct,
		Caps:      gwpb.Caps.CapSet(gwpb.Caps.All()),
		LLBCaps:   opspb.Caps.CapSet(opspb.Caps.All()),
	}
}

func (c *bridgeClient) BuildOpts() client.BuildOpts {
	return c.buildOpts
}

func (c *bridgeClient) Inputs(ctx context.Context) (map[string]llb.State, error) {
	inputs := make(map[string]llb.State)
	for key, def := range c.inputs {
		defop, err := llb.NewDefinitionOp(def)
		if err != nil {
			return nil, err
		}
		inputs[key] = llb.NewState(defop)
	}
	return inputs, nil
}

func (c *bridgeClient) wrapSolveError(solveErr error) error {
	var (
		ee       *llberrdefs.ExecError
		fae      *llberrdefs.FileActionError
		sce      *solver.SlowCacheError
		inputIDs []string
		mountIDs []string
		subject  errdefs.IsSolve_Subject
	)
	if errors.As(solveErr, &ee) {
		var err error
		inputIDs, err = c.registerResultIDs(ee.Inputs...)
		if err != nil {
			return err
		}
		mountIDs, err = c.registerResultIDs(ee.Mounts...)
		if err != nil {
			return err
		}
	}
	if errors.As(solveErr, &fae) {
		subject = fae.ToSubject()
	}
	if errors.As(solveErr, &sce) {
		var err error
		inputIDs, err = c.registerResultIDs(sce.Result)
		if err != nil {
			return err
		}
		subject = sce.ToSubject()
	}
	return errdefs.WithSolveError(solveErr, subject, inputIDs, mountIDs)
}

func (c *bridgeClient) registerResultIDs(results ...solver.Result) (ids []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids = make([]string, len(results))
	for i, res := range results {
		if res == nil {
			continue
		}
		workerRef, ok := res.Sys().(*worker.WorkerRef)
		if !ok {
			return ids, errors.Errorf("unexpected type for result, got %T", res.Sys())
		}
		ids[i] = workerRef.ID()
		c.workerRefByID[workerRef.ID()] = workerRef
	}
	return ids, nil
}

func (c *bridgeClient) toFrontendResult(r *client.Result) (*frontend.Result, error) {
	if r == nil {
		return nil, nil
	}

	res := &frontend.Result{}

	if r.Refs != nil {
		res.Refs = make(map[string]solver.ResultProxy, len(r.Refs))
		for k, r := range r.Refs {
			rr, ok := r.(*ref)
			if !ok {
				return nil, errors.Errorf("invalid reference type for forward %T", r)
			}
			res.Refs[k] = rr.acquireResultProxy()
		}
	}
	if r := r.Ref; r != nil {
		rr, ok := r.(*ref)
		if !ok {
			return nil, errors.Errorf("invalid reference type for forward %T", r)
		}
		res.Ref = rr.acquireResultProxy()
	}
	res.Metadata = r.Metadata
	if r.Attestations != nil {
		res.Attestations = make(map[string][]frontend.Attestation)
		for k, as := range r.Attestations {
			for _, a := range as {
				switch a := a.(type) {
				case *client.InTotoAttestation:
					rr, ok := a.PredicateRef.(*ref)
					if !ok {
						return nil, errors.Errorf("invalid reference type for forward %T", r)
					}

					subjects := make([]attestation.InTotoSubject, len(a.Subjects))
					for i, s := range a.Subjects {
						switch s := s.(type) {
						case *attestation.InTotoSubjectSelf:
							subjects[i] = &attestation.InTotoSubjectSelf{}
						case *attestation.InTotoSubjectRaw:
							subjects[i] = &attestation.InTotoSubjectRaw{
								Name:   s.Name,
								Digest: s.Digest,
							}
						default:
							return nil, errors.Errorf("unknown attestation subject type %T", s)
						}
					}
					res.Attestations[k] = append(res.Attestations[k], &frontend.InTotoAttestation{
						PredicateRef:  rr.acquireResultProxy(),
						PredicatePath: a.PredicatePath,
						PredicateType: a.PredicateType,
						Subjects:      subjects,
					})
				default:
					return nil, errors.Errorf("unknown attestation type %T", a)
				}
			}
		}
	}

	return res, nil
}

func (c *bridgeClient) discard(err error) {
	for _, ctr := range c.ctrs {
		ctr.Release(context.TODO())
	}

	for id, workerRef := range c.workerRefByID {
		workerRef.ImmutableRef.Release(context.TODO())
		delete(c.workerRefByID, id)
	}
	for _, r := range c.refs {
		if r != nil {
			r.resultProxy.Release(context.TODO())
			if err != nil {
				for _, clone := range r.resultProxyClones {
					clone.Release(context.TODO())
				}
			}
		}
	}
}

func (c *bridgeClient) Warn(ctx context.Context, dgst digest.Digest, msg string, opts client.WarnOpts) error {
	return c.FrontendLLBBridge.Warn(ctx, dgst, msg, opts)
}

func (c *bridgeClient) NewContainer(ctx context.Context, req client.NewContainerRequest) (client.Container, error) {
	ctrReq := gateway.NewContainerRequest{
		ContainerID: identity.NewID(),
		NetMode:     req.NetMode,
		Mounts:      make([]gateway.Mount, len(req.Mounts)),
	}

	eg, ctx := errgroup.WithContext(ctx)

	for i, m := range req.Mounts {
		i, m := i, m
		eg.Go(func() error {
			var workerRef *worker.WorkerRef
			if m.Ref != nil {
				refProxy, ok := m.Ref.(*ref)
				if !ok {
					return errors.Errorf("unexpected Ref type: %T", m.Ref)
				}

				res, err := refProxy.resultProxy.Result(ctx)
				if err != nil {
					return err
				}

				workerRef, ok = res.Sys().(*worker.WorkerRef)
				if !ok {
					return errors.Errorf("invalid ref: %T", res.Sys())
				}
			} else if m.ResultID != "" {
				var ok bool
				workerRef, ok = c.workerRefByID[m.ResultID]
				if !ok {
					return errors.Errorf("failed to find ref %s for %q mount", m.ResultID, m.Dest)
				}
			}
			ctrReq.Mounts[i] = gateway.Mount{
				WorkerRef: workerRef,
				Mount: &opspb.Mount{
					Dest:      m.Dest,
					Selector:  m.Selector,
					Readonly:  m.Readonly,
					MountType: m.MountType,
					CacheOpt:  m.CacheOpt,
					SecretOpt: m.SecretOpt,
					SSHOpt:    m.SSHOpt,
				},
			}
			return nil
		})
	}

	err := eg.Wait()
	if err != nil {
		return nil, err
	}

	ctrReq.ExtraHosts, err = gateway.ParseExtraHosts(req.ExtraHosts)
	if err != nil {
		return nil, err
	}

	w, err := c.workers.GetDefault()
	if err != nil {
		return nil, err
	}

	group := session.NewGroup(c.sid)
	ctr, err := gateway.NewContainer(ctx, w, c.sm, group, ctrReq)
	if err != nil {
		return nil, err
	}
	c.ctrs = append(c.ctrs, ctr)
	return ctr, nil
}

func (c *bridgeClient) newRef(r solver.ResultProxy, s session.Group) (*ref, error) {
	return &ref{resultProxy: r, session: s, c: c}, nil
}

func (c *bridgeClient) newAttestation(a frontend.Attestation, s session.Group) (client.Attestation, []*ref, error) {
	switch a := a.(type) {
	case *frontend.InTotoAttestation:
		rr, err := c.newRef(a.PredicateRef, session.NewGroup(c.sid))
		if err != nil {
			return nil, nil, err
		}

		subjects := make([]attestation.InTotoSubject, len(a.Subjects))
		for i, subject := range a.Subjects {
			switch subject := subject.(type) {
			case *attestation.InTotoSubjectSelf:
				subjects[i] = &attestation.InTotoSubjectSelf{}
			case *attestation.InTotoSubjectRaw:
				subjects[i] = &attestation.InTotoSubjectRaw{
					Name:   subject.Name,
					Digest: subject.Digest,
				}
			default:
				return nil, nil, errors.Errorf("unknown attestation subject type %T", s)
			}
		}
		return &client.InTotoAttestation{
			PredicateType: a.PredicateType,
			PredicateRef:  rr,
			PredicatePath: a.PredicatePath,
			Subjects:      subjects,
		}, []*ref{rr}, nil
	default:
		return nil, nil, errors.Errorf("unknown attestation type %T", a)
	}
}

type ref struct {
	resultProxy       solver.ResultProxy
	resultProxyClones []solver.ResultProxy

	session session.Group
	c       *bridgeClient
}

func (r *ref) acquireResultProxy() solver.ResultProxy {
	s1, s2 := solver.SplitResultProxy(r.resultProxy)
	r.resultProxy = s1
	r.resultProxyClones = append(r.resultProxyClones, s2)
	return s2
}

func (r *ref) ToState() (st llb.State, err error) {
	defop, err := llb.NewDefinitionOp(r.resultProxy.Definition())
	if err != nil {
		return st, err
	}
	return llb.NewState(defop), nil
}

func (r *ref) ReadFile(ctx context.Context, req client.ReadRequest) ([]byte, error) {
	m, err := r.getMountable(ctx)
	if err != nil {
		return nil, err
	}
	newReq := cacheutil.ReadRequest{
		Filename: req.Filename,
	}
	if r := req.Range; r != nil {
		newReq.Range = &cacheutil.FileRange{
			Offset: r.Offset,
			Length: r.Length,
		}
	}
	return cacheutil.ReadFile(ctx, m, newReq)
}

func (r *ref) ReadDir(ctx context.Context, req client.ReadDirRequest) ([]*fstypes.Stat, error) {
	m, err := r.getMountable(ctx)
	if err != nil {
		return nil, err
	}
	newReq := cacheutil.ReadDirRequest{
		Path:           req.Path,
		IncludePattern: req.IncludePattern,
	}
	return cacheutil.ReadDir(ctx, m, newReq)
}

func (r *ref) StatFile(ctx context.Context, req client.StatRequest) (*fstypes.Stat, error) {
	m, err := r.getMountable(ctx)
	if err != nil {
		return nil, err
	}
	return cacheutil.StatFile(ctx, m, req.Path)
}

func (r *ref) getMountable(ctx context.Context) (snapshot.Mountable, error) {
	rr, err := r.resultProxy.Result(ctx)
	if err != nil {
		return nil, r.c.wrapSolveError(err)
	}
	ref, ok := rr.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", rr.Sys())
	}
	return ref.ImmutableRef.Mount(ctx, true, r.session)
}
