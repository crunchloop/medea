// Package store is Medea's embedded state store (bbolt). It is the single
// source of truth for *desired* config and in-flight *rollout* progress;
// *observed* state is an in-memory cache, never persisted. See
// design/datastore.md for the full design.
package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/crunchloop/medea/gen/medea/v1"
)

// Revision is the global monotonic write counter. Every successful write bumps
// it and stamps the written record; it drives both watch cursors and
// optimistic concurrency (datastore.md §4).
type Revision uint64

// ErrConflict is returned by a CAS write whose expected revision does not match
// the stored record's current revision.
var ErrConflict = errors.New("store: revision conflict")

// EventKind identifies which resource a watch Event refers to.
type EventKind string

const (
	KindCluster        EventKind = "cluster"
	KindNodePool       EventKind = "nodepool"
	KindMachine        EventKind = "machine"
	KindHost           EventKind = "host"
	KindMachineRollout EventKind = "machine_rollout"
	KindClusterRollout EventKind = "cluster_rollout"
	KindRolloutJob     EventKind = "rollout_job"
)

// Event is published after a persisted write commits (datastore.md §5).
type Event struct {
	Kind     EventKind
	Key      string
	Revision Revision
}

// bucket names
var (
	bMeta     = []byte("meta")
	bDesired  = []byte("desired")
	bRollouts = []byte("rollouts")

	sClusters = []byte("clusters")
	sNodePool = []byte("nodepools")
	sMachines = []byte("machines")
	sHosts    = []byte("hosts")
	sJobs     = []byte("jobs")

	kRevision = []byte("revision")
)

const subBuffer = 256 // per-subscriber buffer; homelab scale (datastore.md §5)

// Store is the typed surface every reconciler and API handler uses. The bbolt
// implementation can be swapped (e.g. for COSI) behind this seam — datastore.md §10.
type Store interface {
	GetCluster(cluster string) (*pb.Cluster, Revision, error)
	ListClusters() ([]*pb.Cluster, error)
	PutClusterDesired(c *pb.Cluster, expected Revision) (Revision, error)

	GetNodePool(cluster, name string) (*pb.NodePool, Revision, error)
	ListNodePools(cluster string) ([]*pb.NodePool, error)
	PutNodePoolDesired(np *pb.NodePool, expected Revision) (Revision, error)

	GetMachine(cluster, addr string) (*pb.Machine, Revision, error)
	ListMachines(cluster, pool string) ([]*pb.Machine, error)
	PutMachineDesired(m *pb.Machine, expected Revision) (Revision, error)
	DeleteMachine(cluster, addr string) error

	GetHost(cluster, mac string) (*pb.Host, Revision, error)
	ListHosts(cluster, pool string) ([]*pb.Host, error)
	PutHostDesired(h *pb.Host, expected Revision) (Revision, error)
	DeleteHost(cluster, mac string) error

	GetMachineRollout(cluster, addr string) (*pb.MachineRollout, error)
	PutMachineRollout(r *pb.MachineRollout) error
	GetClusterRollout(cluster string) (*pb.ClusterRollout, error)
	PutClusterRollout(r *pb.ClusterRollout) error

	GetRolloutJob(cluster, pool string) (*pb.Rollout, error)
	ListRolloutJobs(cluster string) ([]*pb.Rollout, error)
	PutRolloutJob(r *pb.Rollout) error

	SetClusterObserved(cluster string, o *pb.ClusterObserved)
	SetMachineObserved(cluster, addr string, o *pb.MachineObserved)

	Watch(ctx context.Context, since Revision) (<-chan Event, error)

	Export(w io.Writer) error
	Import(r io.Reader) error

	Close() error
}

// BoltStore is the bbolt-backed Store.
type BoltStore struct {
	db *bolt.DB

	// writeMu serializes persisted writes so the revision bump, the record
	// write, and the watch publish happen as one atomic step (and lastRev stays
	// consistent with what subscribers see).
	writeMu sync.Mutex
	lastRev Revision

	// observed is the in-memory cache; never persisted (datastore.md §2).
	obsMu      sync.RWMutex
	obsCluster map[string]*pb.ClusterObserved // key: cluster
	obsMachine map[string]*pb.MachineObserved // key: cluster\x00addr

	subMu   sync.Mutex
	subs    map[int]chan Event
	nextSub int
}

// Open opens (creating if needed) the bbolt file at path and initializes buckets.
func Open(path string) (*BoltStore, error) {
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}
	s := &BoltStore{
		db:         db,
		obsCluster: map[string]*pb.ClusterObserved{},
		obsMachine: map[string]*pb.MachineObserved{},
		subs:       map[int]chan Event{},
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bMeta)
		if err != nil {
			return err
		}
		des, err := tx.CreateBucketIfNotExists(bDesired)
		if err != nil {
			return err
		}
		rol, err := tx.CreateBucketIfNotExists(bRollouts)
		if err != nil {
			return err
		}
		for _, sub := range [][]byte{sClusters, sNodePool, sMachines, sHosts} {
			if _, err := des.CreateBucketIfNotExists(sub); err != nil {
				return err
			}
		}
		for _, sub := range [][]byte{sClusters, sMachines, sJobs} {
			if _, err := rol.CreateBucketIfNotExists(sub); err != nil {
				return err
			}
		}
		s.lastRev = readRev(meta)
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *BoltStore) Close() error { return s.db.Close() }

// --- revision helpers ---

func readRev(meta *bolt.Bucket) Revision {
	v := meta.Get(kRevision)
	if len(v) != 8 {
		return 0
	}
	return Revision(binary.BigEndian.Uint64(v))
}

func bumpRev(tx *bolt.Tx) (Revision, error) {
	meta := tx.Bucket(bMeta)
	next := readRev(meta) + 1
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(next))
	if err := meta.Put(kRevision, buf[:]); err != nil {
		return 0, err
	}
	return next, nil
}

// compound key: <a>\x00<b>
func ckey(a, b string) []byte { return []byte(a + "\x00" + b) }

// --- generic CAS write ---

// putCAS performs a compare-and-swap write of a record into desired/<sub>.
// expected is the revision the caller last observed (0 to create). It returns
// the new revision and, via emit, the event to publish post-commit.
func (s *BoltStore) putCAS(sub, key []byte, expected Revision, msg proto.Message, kind EventKind, eventKey string) (Revision, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var newRev Revision
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bDesired).Bucket(sub)
		if cur := b.Get(key); cur != nil {
			curRev, err := decodeRevision(cur, msg)
			if err != nil {
				return err
			}
			if curRev != expected {
				return ErrConflict
			}
		} else if expected != 0 {
			return ErrConflict
		}
		r, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = r
		raw, err := marshalWithRev(msg, r)
		if err != nil {
			return err
		}
		return b.Put(key, raw)
	})
	if err != nil {
		return 0, err
	}
	s.lastRev = newRev
	s.publish(Event{Kind: kind, Key: eventKey, Revision: newRev})
	return newRev, nil
}

// putLWW performs a last-writer-wins write into rollouts/<sub> (single logical
// owner per record, so no CAS — datastore.md §6).
func (s *BoltStore) putLWW(sub, key []byte, msg proto.Message, kind EventKind, eventKey string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var newRev Revision
	err := s.db.Update(func(tx *bolt.Tx) error {
		r, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = r
		raw, err := marshalWithRev(msg, r)
		if err != nil {
			return err
		}
		return tx.Bucket(bRollouts).Bucket(sub).Put(key, raw)
	})
	if err != nil {
		return err
	}
	s.lastRev = newRev
	s.publish(Event{Kind: kind, Key: eventKey, Revision: newRev})
	return nil
}

// marshalWithRev clones msg, stamps its revision field to rev, clears any
// observed cache field (never persisted), and marshals to proto bytes.
func marshalWithRev(msg proto.Message, rev Revision) ([]byte, error) {
	clone := proto.Clone(msg)
	switch m := clone.(type) {
	case *pb.Cluster:
		m.Revision = uint64(rev)
		m.Observed = nil
	case *pb.NodePool:
		m.Revision = uint64(rev)
	case *pb.Machine:
		m.Revision = uint64(rev)
		m.Observed = nil
	case *pb.Host:
		m.Revision = uint64(rev)
	case *pb.MachineRollout:
		m.Revision = uint64(rev)
	case *pb.ClusterRollout:
		m.Revision = uint64(rev)
	case *pb.Rollout:
		m.Revision = uint64(rev)
	default:
		return nil, fmt.Errorf("store: unsupported message %T", msg)
	}
	return proto.Marshal(clone)
}

// decodeRevision reads the revision field from stored bytes, decoding against
// the same concrete type as like (the message being written). Decoding against
// the known type is required: proto unmarshal is lenient across message types
// (field numbers get reinterpreted), so guessing the type can read the wrong field.
func decodeRevision(raw []byte, like proto.Message) (Revision, error) {
	m := like.ProtoReflect().New().Interface()
	if err := proto.Unmarshal(raw, m); err != nil {
		return 0, err
	}
	switch v := m.(type) {
	case *pb.Cluster:
		return Revision(v.Revision), nil
	case *pb.NodePool:
		return Revision(v.Revision), nil
	case *pb.Machine:
		return Revision(v.Revision), nil
	case *pb.Host:
		return Revision(v.Revision), nil
	default:
		return 0, fmt.Errorf("store: unsupported message %T", m)
	}
}

// --- Cluster ---

func (s *BoltStore) GetCluster(cluster string) (*pb.Cluster, Revision, error) {
	var c *pb.Cluster
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bDesired).Bucket(sClusters).Get([]byte(cluster))
		if raw == nil {
			return nil
		}
		c = &pb.Cluster{}
		return proto.Unmarshal(raw, c)
	})
	if err != nil || c == nil {
		return nil, 0, err
	}
	s.obsMu.RLock()
	c.Observed = cloneClusterObs(s.obsCluster[cluster])
	s.obsMu.RUnlock()
	return c, Revision(c.Revision), nil
}

func (s *BoltStore) ListClusters() ([]*pb.Cluster, error) {
	var out []*pb.Cluster
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bDesired).Bucket(sClusters).ForEach(func(k, v []byte) error {
			c := &pb.Cluster{}
			if err := proto.Unmarshal(v, c); err != nil {
				return err
			}
			s.obsMu.RLock()
			c.Observed = cloneClusterObs(s.obsCluster[string(k)])
			s.obsMu.RUnlock()
			out = append(out, c)
			return nil
		})
	})
	return out, err
}

func (s *BoltStore) PutClusterDesired(c *pb.Cluster, expected Revision) (Revision, error) {
	if c.GetName() == "" {
		return 0, errors.New("store: cluster name required")
	}
	return s.putCAS(sClusters, []byte(c.Name), expected, c, KindCluster, c.Name)
}

// --- NodePool ---

func (s *BoltStore) GetNodePool(cluster, name string) (*pb.NodePool, Revision, error) {
	var np *pb.NodePool
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bDesired).Bucket(sNodePool).Get(ckey(cluster, name))
		if raw == nil {
			return nil
		}
		np = &pb.NodePool{}
		return proto.Unmarshal(raw, np)
	})
	if err != nil || np == nil {
		return nil, 0, err
	}
	return np, Revision(np.Revision), nil
}

func (s *BoltStore) ListNodePools(cluster string) ([]*pb.NodePool, error) {
	var out []*pb.NodePool
	prefix := []byte(cluster + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bDesired).Bucket(sNodePool).ForEach(func(k, v []byte) error {
			if !hasPrefix(k, prefix) {
				return nil
			}
			np := &pb.NodePool{}
			if err := proto.Unmarshal(v, np); err != nil {
				return err
			}
			out = append(out, np)
			return nil
		})
	})
	return out, err
}

func (s *BoltStore) PutNodePoolDesired(np *pb.NodePool, expected Revision) (Revision, error) {
	if np.GetCluster() == "" || np.GetName() == "" {
		return 0, errors.New("store: nodepool cluster and name required")
	}
	return s.putCAS(sNodePool, ckey(np.Cluster, np.Name), expected, np, KindNodePool, np.Cluster+"/"+np.Name)
}

// --- Machine ---

func (s *BoltStore) GetMachine(cluster, addr string) (*pb.Machine, Revision, error) {
	var m *pb.Machine
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bDesired).Bucket(sMachines).Get(ckey(cluster, addr))
		if raw == nil {
			return nil
		}
		m = &pb.Machine{}
		return proto.Unmarshal(raw, m)
	})
	if err != nil || m == nil {
		return nil, 0, err
	}
	s.obsMu.RLock()
	m.Observed = cloneMachineObs(s.obsMachine[cluster+"\x00"+addr])
	s.obsMu.RUnlock()
	return m, Revision(m.Revision), nil
}

func (s *BoltStore) ListMachines(cluster, pool string) ([]*pb.Machine, error) {
	var out []*pb.Machine
	prefix := []byte(cluster + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bDesired).Bucket(sMachines).ForEach(func(k, v []byte) error {
			if !hasPrefix(k, prefix) {
				return nil
			}
			m := &pb.Machine{}
			if err := proto.Unmarshal(v, m); err != nil {
				return err
			}
			if pool != "" && m.GetPool() != pool {
				return nil
			}
			s.obsMu.RLock()
			m.Observed = cloneMachineObs(s.obsMachine[string(k)])
			s.obsMu.RUnlock()
			out = append(out, m)
			return nil
		})
	})
	return out, err
}

func (s *BoltStore) PutMachineDesired(m *pb.Machine, expected Revision) (Revision, error) {
	if m.GetCluster() == "" || m.GetTalosEndpoint() == "" {
		return 0, errors.New("store: machine cluster and talos_endpoint required")
	}
	return s.putCAS(sMachines, ckey(m.Cluster, m.TalosEndpoint), expected, m, KindMachine, m.Cluster+"/"+m.TalosEndpoint)
}

// --- Host (provisioning inventory; desired/precious; CAS — provisioning-plane.md §2) ---

func (s *BoltStore) GetHost(cluster, mac string) (*pb.Host, Revision, error) {
	var h *pb.Host
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bDesired).Bucket(sHosts).Get(ckey(cluster, mac))
		if raw == nil {
			return nil
		}
		h = &pb.Host{}
		return proto.Unmarshal(raw, h)
	})
	if err != nil || h == nil {
		return nil, 0, err
	}
	return h, Revision(h.Revision), nil
}

func (s *BoltStore) ListHosts(cluster, pool string) ([]*pb.Host, error) {
	var out []*pb.Host
	prefix := []byte(cluster + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bDesired).Bucket(sHosts).ForEach(func(k, v []byte) error {
			if !hasPrefix(k, prefix) {
				return nil
			}
			h := &pb.Host{}
			if err := proto.Unmarshal(v, h); err != nil {
				return err
			}
			if pool != "" && h.GetPool() != pool {
				return nil
			}
			out = append(out, h)
			return nil
		})
	})
	return out, err
}

func (s *BoltStore) PutHostDesired(h *pb.Host, expected Revision) (Revision, error) {
	if h.GetCluster() == "" || h.GetMac() == "" {
		return 0, errors.New("store: host cluster and mac required")
	}
	return s.putCAS(sHosts, ckey(h.Cluster, h.Mac), expected, h, KindHost, h.Cluster+"/"+h.Mac)
}

func (s *BoltStore) DeleteHost(cluster, mac string) error {
	if cluster == "" || mac == "" {
		return errors.New("store: host cluster and mac required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var newRev Revision
	err := s.db.Update(func(tx *bolt.Tx) error {
		r, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = r
		return tx.Bucket(bDesired).Bucket(sHosts).Delete(ckey(cluster, mac))
	})
	if err != nil {
		return err
	}
	s.lastRev = newRev
	s.publish(Event{Kind: KindHost, Key: cluster + "/" + mac, Revision: newRev})
	return nil
}

// DeleteMachine removes a machine identity record (used by provisioning scale-in)
// and clears its in-memory observed entry.
func (s *BoltStore) DeleteMachine(cluster, addr string) error {
	if cluster == "" || addr == "" {
		return errors.New("store: machine cluster and addr required")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var newRev Revision
	err := s.db.Update(func(tx *bolt.Tx) error {
		r, err := bumpRev(tx)
		if err != nil {
			return err
		}
		newRev = r
		return tx.Bucket(bDesired).Bucket(sMachines).Delete(ckey(cluster, addr))
	})
	if err != nil {
		return err
	}
	s.obsMu.Lock()
	delete(s.obsMachine, cluster+"\x00"+addr)
	s.obsMu.Unlock()
	s.lastRev = newRev
	s.publish(Event{Kind: KindMachine, Key: cluster + "/" + addr, Revision: newRev})
	return nil
}

// --- Rollouts (LWW) ---

func (s *BoltStore) GetMachineRollout(cluster, addr string) (*pb.MachineRollout, error) {
	var r *pb.MachineRollout
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bRollouts).Bucket(sMachines).Get(ckey(cluster, addr))
		if raw == nil {
			return nil
		}
		r = &pb.MachineRollout{}
		return proto.Unmarshal(raw, r)
	})
	return r, err
}

func (s *BoltStore) PutMachineRollout(r *pb.MachineRollout) error {
	if r.GetCluster() == "" || r.GetAddr() == "" {
		return errors.New("store: machine rollout cluster and addr required")
	}
	return s.putLWW(sMachines, ckey(r.Cluster, r.Addr), r, KindMachineRollout, r.Cluster+"/"+r.Addr)
}

func (s *BoltStore) GetClusterRollout(cluster string) (*pb.ClusterRollout, error) {
	var r *pb.ClusterRollout
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bRollouts).Bucket(sClusters).Get([]byte(cluster))
		if raw == nil {
			return nil
		}
		r = &pb.ClusterRollout{}
		return proto.Unmarshal(raw, r)
	})
	return r, err
}

func (s *BoltStore) PutClusterRollout(r *pb.ClusterRollout) error {
	if r.GetCluster() == "" {
		return errors.New("store: cluster rollout cluster required")
	}
	return s.putLWW(sClusters, []byte(r.Cluster), r, KindClusterRollout, r.Cluster)
}

// --- Rollout jobs (LWW; one active job per cluster/pool) ---

func (s *BoltStore) GetRolloutJob(cluster, pool string) (*pb.Rollout, error) {
	var r *pb.Rollout
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bRollouts).Bucket(sJobs).Get(ckey(cluster, pool))
		if raw == nil {
			return nil
		}
		r = &pb.Rollout{}
		return proto.Unmarshal(raw, r)
	})
	return r, err
}

func (s *BoltStore) ListRolloutJobs(cluster string) ([]*pb.Rollout, error) {
	var out []*pb.Rollout
	prefix := []byte(cluster + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bRollouts).Bucket(sJobs).ForEach(func(k, v []byte) error {
			if !hasPrefix(k, prefix) {
				return nil
			}
			r := &pb.Rollout{}
			if err := proto.Unmarshal(v, r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *BoltStore) PutRolloutJob(r *pb.Rollout) error {
	if r.GetCluster() == "" {
		return errors.New("store: rollout cluster required")
	}
	return s.putLWW(sJobs, ckey(r.Cluster, r.Pool), r, KindRolloutJob, r.Cluster+"/"+r.Pool)
}

// --- Observed (in-memory cache) ---

func (s *BoltStore) SetClusterObserved(cluster string, o *pb.ClusterObserved) {
	s.obsMu.Lock()
	s.obsCluster[cluster] = cloneClusterObs(o)
	s.obsMu.Unlock()
}

func (s *BoltStore) SetMachineObserved(cluster, addr string, o *pb.MachineObserved) {
	s.obsMu.Lock()
	s.obsMachine[cluster+"\x00"+addr] = cloneMachineObs(o)
	s.obsMu.Unlock()
}

func cloneClusterObs(o *pb.ClusterObserved) *pb.ClusterObserved {
	if o == nil {
		return nil
	}
	return proto.Clone(o).(*pb.ClusterObserved)
}

func cloneMachineObs(o *pb.MachineObserved) *pb.MachineObserved {
	if o == nil {
		return nil
	}
	return proto.Clone(o).(*pb.MachineObserved)
}

func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}
