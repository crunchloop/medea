// Package server implements the Medea gRPC service over a store.Store
// (design/api-and-auth.md). Mutations are server-side read-modify-write with
// optional compare-and-swap; reads return the proto domain types directly.
package server

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/bilby91/medea/gen/medea/v1"
	"github.com/bilby91/medea/internal/store"
)

// Server is the gRPC service implementation.
type Server struct {
	pb.UnimplementedMedeaServer
	store store.Store
}

// New returns a Server backed by st.
func New(st store.Store) *Server { return &Server{store: st} }

// mapErr converts non-domain store errors to gRPC status codes.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrConflict):
		return status.Error(codes.Aborted, "write contention")
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

// --- reads ---

func (s *Server) GetCluster(_ context.Context, req *pb.GetClusterRequest) (*pb.Cluster, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	c, _, err := s.store.GetCluster(req.GetCluster())
	if err != nil {
		return nil, mapErr(err)
	}
	if c == nil {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.GetCluster())
	}
	return c, nil
}

func (s *Server) ListClusters(_ context.Context, _ *pb.ListClustersRequest) (*pb.ListClustersResponse, error) {
	cs, err := s.store.ListClusters()
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.ListClustersResponse{Clusters: cs}, nil
}

func (s *Server) ListNodePools(_ context.Context, req *pb.ListNodePoolsRequest) (*pb.ListNodePoolsResponse, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	nps, err := s.store.ListNodePools(req.GetCluster())
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.ListNodePoolsResponse{NodePools: nps}, nil
}

func (s *Server) ListMachines(_ context.Context, req *pb.ListMachinesRequest) (*pb.ListMachinesResponse, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	ms, err := s.store.ListMachines(req.GetCluster(), req.GetPool())
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.ListMachinesResponse{Machines: ms}, nil
}

func (s *Server) GetRollout(_ context.Context, req *pb.GetRolloutRequest) (*pb.GetRolloutResponse, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	cr, err := s.store.GetClusterRollout(req.GetCluster())
	if err != nil {
		return nil, mapErr(err)
	}
	ms, err := s.store.ListMachines(req.GetCluster(), req.GetPool())
	if err != nil {
		return nil, mapErr(err)
	}
	var rollouts []*pb.MachineRollout
	for _, m := range ms {
		mr, err := s.store.GetMachineRollout(req.GetCluster(), m.GetTalosEndpoint())
		if err != nil {
			return nil, mapErr(err)
		}
		if mr != nil {
			rollouts = append(rollouts, mr)
		}
	}
	return &pb.GetRolloutResponse{ClusterRollout: cr, MachineRollouts: rollouts}, nil
}

// --- mutations (read-modify-write + optional CAS) ---

func (s *Server) SetClusterVersions(_ context.Context, req *pb.SetClusterVersionsRequest) (*pb.SetVersionsResponse, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	rev, err := rmw(func() (store.Revision, error) {
		c, rev, err := s.store.GetCluster(req.GetCluster())
		if err != nil {
			return 0, err
		}
		if c == nil {
			return 0, status.Errorf(codes.NotFound, "cluster %q not found", req.GetCluster())
		}
		if req.GetExpectedRevision() != 0 && uint64(rev) != req.GetExpectedRevision() {
			return 0, status.Errorf(codes.FailedPrecondition, "revision mismatch: have %d, want %d", rev, req.GetExpectedRevision())
		}
		if c.Desired == nil {
			c.Desired = &pb.ClusterDesired{}
		}
		if req.TalosVersion != nil {
			c.Desired.TalosVersion = req.GetTalosVersion()
		}
		if req.KubernetesVersion != nil {
			c.Desired.KubernetesVersion = req.GetKubernetesVersion()
		}
		return s.store.PutClusterDesired(c, rev)
	}, req.GetExpectedRevision())
	if err != nil {
		return nil, err
	}
	return &pb.SetVersionsResponse{Revision: uint64(rev)}, nil
}

func (s *Server) SetNodePoolVersion(_ context.Context, req *pb.SetNodePoolVersionRequest) (*pb.SetVersionsResponse, error) {
	if req.GetCluster() == "" || req.GetPool() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster and pool required")
	}
	rev, err := rmw(func() (store.Revision, error) {
		np, rev, err := s.store.GetNodePool(req.GetCluster(), req.GetPool())
		if err != nil {
			return 0, err
		}
		if np == nil {
			return 0, status.Errorf(codes.NotFound, "nodepool %q/%q not found", req.GetCluster(), req.GetPool())
		}
		if req.GetExpectedRevision() != 0 && uint64(rev) != req.GetExpectedRevision() {
			return 0, status.Errorf(codes.FailedPrecondition, "revision mismatch: have %d, want %d", rev, req.GetExpectedRevision())
		}
		if np.Desired == nil {
			np.Desired = &pb.NodePoolDesired{}
		}
		if req.TalosVersion != nil {
			np.Desired.TalosVersion = req.GetTalosVersion()
		}
		return s.store.PutNodePoolDesired(np, rev)
	}, req.GetExpectedRevision())
	if err != nil {
		return nil, err
	}
	return &pb.SetVersionsResponse{Revision: uint64(rev)}, nil
}

func (s *Server) PauseRollout(_ context.Context, req *pb.PauseRolloutRequest) (*pb.RolloutControlResponse, error) {
	rev, err := s.setPaused(req.GetCluster(), req.GetPool(), true)
	if err != nil {
		return nil, err
	}
	return &pb.RolloutControlResponse{Revision: uint64(rev)}, nil
}

func (s *Server) ResumeRollout(_ context.Context, req *pb.ResumeRolloutRequest) (*pb.RolloutControlResponse, error) {
	rev, err := s.setPaused(req.GetCluster(), req.GetPool(), false)
	if err != nil {
		return nil, err
	}
	return &pb.RolloutControlResponse{Revision: uint64(rev)}, nil
}

func (s *Server) setPaused(cluster, pool string, paused bool) (store.Revision, error) {
	if cluster == "" || pool == "" {
		return 0, status.Error(codes.InvalidArgument, "cluster and pool required")
	}
	return rmw(func() (store.Revision, error) {
		np, rev, err := s.store.GetNodePool(cluster, pool)
		if err != nil {
			return 0, err
		}
		if np == nil {
			return 0, status.Errorf(codes.NotFound, "nodepool %q/%q not found", cluster, pool)
		}
		np.Paused = paused
		return s.store.PutNodePoolDesired(np, rev)
	}, 0)
}

// rmw runs a read-modify-write body. With no CAS pin (expected==0) it retries
// once on a lost race (store.ErrConflict); with a pin it surfaces the conflict
// as Aborted. Status errors from the body pass through unchanged.
func rmw(body func() (store.Revision, error), expected uint64) (store.Revision, error) {
	for attempt := 0; attempt < 2; attempt++ {
		rev, err := body()
		if err == nil {
			return rev, nil
		}
		if errors.Is(err, store.ErrConflict) {
			if expected != 0 {
				return 0, status.Error(codes.Aborted, "lost race against concurrent write")
			}
			continue // retry the read-modify-write once
		}
		if _, ok := status.FromError(err); ok {
			return 0, err // already a status error (NotFound/FailedPrecondition/...)
		}
		return 0, mapErr(err)
	}
	return 0, status.Error(codes.Aborted, "write contention")
}

// --- rollout safety (design/rollout-safety.md) ---

func (s *Server) EnableRollouts(_ context.Context, req *pb.EnableRolloutsRequest) (*pb.Cluster, error) {
	return s.setRolloutsEnabled(req.GetCluster(), true)
}

func (s *Server) DisableRollouts(_ context.Context, req *pb.EnableRolloutsRequest) (*pb.Cluster, error) {
	return s.setRolloutsEnabled(req.GetCluster(), false)
}

func (s *Server) setRolloutsEnabled(cluster string, enabled bool) (*pb.Cluster, error) {
	if cluster == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	var result *pb.Cluster
	_, err := rmw(func() (store.Revision, error) {
		c, rev, err := s.store.GetCluster(cluster)
		if err != nil {
			return 0, err
		}
		if c == nil {
			return 0, status.Errorf(codes.NotFound, "cluster %q not found", cluster)
		}
		c.RolloutsEnabled = enabled
		nr, err := s.store.PutClusterDesired(c, rev)
		if err != nil {
			return 0, err
		}
		c.Revision = uint64(nr)
		result = c
		return nr, nil
	}, 0)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CreateRollout authorizes and records an upgrade. It enforces the full guard
// chain (rollout-safety.md §6) before anything is written.
func (s *Server) CreateRollout(_ context.Context, req *pb.CreateRolloutRequest) (*pb.Rollout, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	if req.GetTargetVersion() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_version required")
	}

	c, _, err := s.store.GetCluster(req.GetCluster())
	if err != nil {
		return nil, mapErr(err)
	}
	if c == nil {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.GetCluster())
	}
	if !c.GetRolloutsEnabled() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"rollouts not enabled for cluster %q; run `medea cluster enable-rollouts %s`", req.GetCluster(), req.GetCluster())
	}
	if c.GetMode() == pb.ClusterMode_CLUSTER_MODE_AUTO {
		return nil, status.Error(codes.Unimplemented, "auto (drift-reconcile) mode is not supported in v1")
	}

	switch req.GetKind() {
	case pb.RolloutKind_ROLLOUT_KIND_TALOS:
		return s.createTalosRollout(req)
	case pb.RolloutKind_ROLLOUT_KIND_KUBERNETES:
		return s.createKubernetesRollout(req)
	default:
		return nil, status.Error(codes.InvalidArgument, "kind must be TALOS or KUBERNETES")
	}
}

func (s *Server) createTalosRollout(req *pb.CreateRolloutRequest) (*pb.Rollout, error) {
	if req.GetPool() == "" {
		return nil, status.Error(codes.InvalidArgument, "pool required for a talos rollout")
	}
	np, _, err := s.store.GetNodePool(req.GetCluster(), req.GetPool())
	if err != nil {
		return nil, mapErr(err)
	}
	if np == nil {
		return nil, status.Errorf(codes.NotFound, "nodepool %q/%q not found", req.GetCluster(), req.GetPool())
	}

	// Set desired = target (so the reconciler converges to it), then record the
	// job. Both are guarded by the checks above.
	if _, err := rmw(func() (store.Revision, error) {
		cur, rev, err := s.store.GetNodePool(req.GetCluster(), req.GetPool())
		if err != nil {
			return 0, err
		}
		if cur == nil {
			return 0, status.Errorf(codes.NotFound, "nodepool %q/%q not found", req.GetCluster(), req.GetPool())
		}
		if cur.Desired == nil {
			cur.Desired = &pb.NodePoolDesired{}
		}
		cur.Desired.TalosVersion = req.GetTargetVersion()
		return s.store.PutNodePoolDesired(cur, rev)
	}, 0); err != nil {
		return nil, err
	}

	job := &pb.Rollout{
		Cluster:        req.GetCluster(),
		Pool:           req.GetPool(),
		Kind:           pb.RolloutKind_ROLLOUT_KIND_TALOS,
		TargetVersion:  req.GetTargetVersion(),
		State:          pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING,
		CreatedBy:      req.GetCreatedBy(),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		PlannedTargets: np.GetMembers(),
	}
	if err := s.store.PutRolloutJob(job); err != nil {
		return nil, mapErr(err)
	}
	saved, err := s.store.GetRolloutJob(req.GetCluster(), req.GetPool())
	if err != nil {
		return nil, mapErr(err)
	}
	return saved, nil
}

// createKubernetesRollout authorizes a cluster-wide Kubernetes upgrade. K8s
// upgrades are orchestrated by Talos itself (not node-by-node), so they are
// cluster-scoped: the pool must be empty. Guards (cluster exists, enabled,
// manual mode, valid target) are enforced by the caller (CreateRollout).
func (s *Server) createKubernetesRollout(req *pb.CreateRolloutRequest) (*pb.Rollout, error) {
	if req.GetPool() != "" {
		return nil, status.Error(codes.InvalidArgument, "kubernetes rollouts are cluster-wide; omit pool")
	}

	// Set desired = target (so the reconciler converges to it), guarded above.
	if _, err := rmw(func() (store.Revision, error) {
		c, rev, err := s.store.GetCluster(req.GetCluster())
		if err != nil {
			return 0, err
		}
		if c == nil {
			return 0, status.Errorf(codes.NotFound, "cluster %q not found", req.GetCluster())
		}
		if c.Desired == nil {
			c.Desired = &pb.ClusterDesired{}
		}
		c.Desired.KubernetesVersion = req.GetTargetVersion()
		return s.store.PutClusterDesired(c, rev)
	}, 0); err != nil {
		return nil, err
	}

	// Planned targets = all cluster machines (informational; Talos upgrades the
	// whole cluster). Captured at plan time so the recorded scope can't expand.
	machines, err := s.store.ListMachines(req.GetCluster(), "")
	if err != nil {
		return nil, mapErr(err)
	}
	targets := make([]string, 0, len(machines))
	for _, m := range machines {
		targets = append(targets, m.GetTalosEndpoint())
	}

	job := &pb.Rollout{
		Cluster:        req.GetCluster(),
		Pool:           "", // cluster-wide
		Kind:           pb.RolloutKind_ROLLOUT_KIND_KUBERNETES,
		TargetVersion:  req.GetTargetVersion(),
		State:          pb.RolloutJobState_ROLLOUT_JOB_STATE_PENDING,
		CreatedBy:      req.GetCreatedBy(),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		PlannedTargets: targets,
	}
	if err := s.store.PutRolloutJob(job); err != nil {
		return nil, mapErr(err)
	}
	saved, err := s.store.GetRolloutJob(req.GetCluster(), "")
	if err != nil {
		return nil, mapErr(err)
	}
	return saved, nil
}

func (s *Server) ListRollouts(_ context.Context, req *pb.ListRolloutsRequest) (*pb.ListRolloutsResponse, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	jobs, err := s.store.ListRolloutJobs(req.GetCluster())
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.ListRolloutsResponse{Rollouts: jobs}, nil
}

// --- provisioning inventory (v2, design/provisioning-plane.md) ---

func (s *Server) ListHosts(_ context.Context, req *pb.ListHostsRequest) (*pb.ListHostsResponse, error) {
	if req.GetCluster() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster required")
	}
	hs, err := s.store.ListHosts(req.GetCluster(), req.GetPool())
	if err != nil {
		return nil, mapErr(err)
	}
	return &pb.ListHostsResponse{Hosts: hs}, nil
}

// RegisterHost records a bare-metal host (by MAC) in the inventory as
// REGISTERED. It is an upsert: re-registering updates pool/role/labels but
// preserves a lifecycle state already advanced by the (future) provisioning
// reconciler. v2-M1 only registers — nothing is provisioned yet.
func (s *Server) RegisterHost(_ context.Context, req *pb.RegisterHostRequest) (*pb.Host, error) {
	if req.GetCluster() == "" || req.GetMac() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster and mac required")
	}
	c, _, err := s.store.GetCluster(req.GetCluster())
	if err != nil {
		return nil, mapErr(err)
	}
	if c == nil {
		return nil, status.Errorf(codes.NotFound, "cluster %q not found", req.GetCluster())
	}
	role := req.GetRole()
	if req.GetPool() != "" {
		np, _, err := s.store.GetNodePool(req.GetCluster(), req.GetPool())
		if err != nil {
			return nil, mapErr(err)
		}
		if np == nil {
			return nil, status.Errorf(codes.NotFound, "nodepool %q/%q not found", req.GetCluster(), req.GetPool())
		}
		if role == pb.Role_ROLE_UNSPECIFIED {
			role = np.GetRole()
		}
	}

	var saved *pb.Host
	if _, err := rmw(func() (store.Revision, error) {
		cur, rev, err := s.store.GetHost(req.GetCluster(), req.GetMac())
		if err != nil {
			return 0, err
		}
		h := &pb.Host{
			Cluster: req.GetCluster(),
			Mac:     req.GetMac(),
			Pool:    req.GetPool(),
			Role:    role,
			Labels:  req.GetLabels(),
			State:   pb.HostState_HOST_STATE_REGISTERED,
		}
		if cur != nil && cur.GetState() != pb.HostState_HOST_STATE_UNSPECIFIED {
			h.State = cur.GetState() // preserve a reconciler-advanced lifecycle state
			h.Addr = cur.GetAddr()
		}
		nr, err := s.store.PutHostDesired(h, rev)
		if err != nil {
			return 0, err
		}
		h.Revision = uint64(nr)
		saved = h
		return nr, nil
	}, 0); err != nil {
		return nil, err
	}
	return saved, nil
}

func (s *Server) DeregisterHost(_ context.Context, req *pb.DeregisterHostRequest) (*pb.DeregisterHostResponse, error) {
	if req.GetCluster() == "" || req.GetMac() == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster and mac required")
	}
	if err := s.store.DeleteHost(req.GetCluster(), req.GetMac()); err != nil {
		return nil, mapErr(err)
	}
	return &pb.DeregisterHostResponse{}, nil
}

// --- watch ---

func (s *Server) Watch(req *pb.WatchRequest, stream pb.Medea_WatchServer) error {
	ch, err := s.store.Watch(stream.Context(), store.Revision(req.GetSinceRevision()))
	if err != nil {
		return mapErr(err)
	}
	for ev := range ch {
		if err := stream.Send(&pb.WatchEvent{
			Kind:     string(ev.Kind),
			Key:      ev.Key,
			Revision: uint64(ev.Revision),
		}); err != nil {
			return err
		}
	}
	return nil
}
