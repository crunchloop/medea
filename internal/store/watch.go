package store

import (
	"context"
	"encoding/json"
	"io"
	"sort"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/bilby91/medea/gen/medea/v1"
)

// publish fans an event out to all subscribers, dropping on a full buffer
// (datastore.md §5: slow/disconnected watchers re-snapshot on reconnect).
func (s *BoltStore) publish(ev Event) {
	s.subMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.subMu.Unlock()
}

// Watch streams change Events. It first replays a consistent snapshot of records
// with revision in (since, snapshotRev], then forwards live events with
// revision > snapshotRev — no gap, no duplicate (datastore.md §5).
func (s *BoltStore) Watch(ctx context.Context, since Revision) (<-chan Event, error) {
	// Register under writeMu so no publish can interleave between capturing
	// snapshotRev and registering the subscriber.
	s.writeMu.Lock()
	snapshotRev := s.lastRev
	in := make(chan Event, subBuffer)
	s.subMu.Lock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = in
	s.subMu.Unlock()
	s.writeMu.Unlock()

	out := make(chan Event, subBuffer)
	go func() {
		defer close(out)
		defer func() {
			s.subMu.Lock()
			delete(s.subs, id)
			s.subMu.Unlock()
		}()

		snap, err := s.snapshotEvents(since, snapshotRev)
		if err == nil {
			for _, ev := range snap {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
		for {
			select {
			case ev := <-in:
				if ev.Revision <= snapshotRev {
					continue // already covered by the snapshot
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// snapshotEvents returns one event per persisted record whose revision is in
// (since, upto], ordered by revision ascending.
func (s *BoltStore) snapshotEvents(since, upto Revision) ([]Event, error) {
	var evs []Event
	add := func(kind EventKind, key string, rev Revision) {
		if rev > since && rev <= upto {
			evs = append(evs, Event{Kind: kind, Key: key, Revision: rev})
		}
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		des := tx.Bucket(bDesired)
		if err := des.Bucket(sClusters).ForEach(func(_, v []byte) error {
			c := &pb.Cluster{}
			if err := proto.Unmarshal(v, c); err != nil {
				return err
			}
			add(KindCluster, c.Name, Revision(c.Revision))
			return nil
		}); err != nil {
			return err
		}
		if err := des.Bucket(sNodePool).ForEach(func(_, v []byte) error {
			np := &pb.NodePool{}
			if err := proto.Unmarshal(v, np); err != nil {
				return err
			}
			add(KindNodePool, np.Cluster+"/"+np.Name, Revision(np.Revision))
			return nil
		}); err != nil {
			return err
		}
		if err := des.Bucket(sMachines).ForEach(func(_, v []byte) error {
			m := &pb.Machine{}
			if err := proto.Unmarshal(v, m); err != nil {
				return err
			}
			add(KindMachine, m.Cluster+"/"+m.TalosEndpoint, Revision(m.Revision))
			return nil
		}); err != nil {
			return err
		}
		rol := tx.Bucket(bRollouts)
		if err := rol.Bucket(sMachines).ForEach(func(_, v []byte) error {
			r := &pb.MachineRollout{}
			if err := proto.Unmarshal(v, r); err != nil {
				return err
			}
			add(KindMachineRollout, r.Cluster+"/"+r.Addr, Revision(r.Revision))
			return nil
		}); err != nil {
			return err
		}
		return rol.Bucket(sClusters).ForEach(func(_, v []byte) error {
			r := &pb.ClusterRollout{}
			if err := proto.Unmarshal(v, r); err != nil {
				return err
			}
			add(KindClusterRollout, r.Cluster, Revision(r.Revision))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Revision < evs[j].Revision })
	return evs, nil
}

// exportDoc is the on-disk backup shape: desired state only (datastore.md §9).
// No credentials, no observed, no rollout progress.
type exportDoc struct {
	Clusters  []json.RawMessage `json:"clusters"`
	NodePools []json.RawMessage `json:"nodePools"`
	Machines  []json.RawMessage `json:"machines"`
}

// Export writes the desired state as a human-readable JSON document.
func (s *BoltStore) Export(w io.Writer) error {
	doc := exportDoc{}
	clusters, err := s.ListClusters()
	if err != nil {
		return err
	}
	for _, c := range clusters {
		c.Observed = nil
		raw, err := protojson.Marshal(c)
		if err != nil {
			return err
		}
		doc.Clusters = append(doc.Clusters, raw)
		nps, err := s.ListNodePools(c.Name)
		if err != nil {
			return err
		}
		for _, np := range nps {
			raw, err := protojson.Marshal(np)
			if err != nil {
				return err
			}
			doc.NodePools = append(doc.NodePools, raw)
		}
		ms, err := s.ListMachines(c.Name, "")
		if err != nil {
			return err
		}
		for _, m := range ms {
			m.Observed = nil
			raw, err := protojson.Marshal(m)
			if err != nil {
				return err
			}
			doc.Machines = append(doc.Machines, raw)
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Import restores desired state from an Export document, overwriting any
// existing records of the same identity (restore semantics, datastore.md §9).
func (s *BoltStore) Import(r io.Reader) error {
	var doc exportDoc
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return err
	}
	for _, raw := range doc.Clusters {
		c := &pb.Cluster{}
		if err := protojson.Unmarshal(raw, c); err != nil {
			return err
		}
		_, rev, err := s.GetCluster(c.Name)
		if err != nil {
			return err
		}
		if _, err := s.PutClusterDesired(c, rev); err != nil {
			return err
		}
	}
	for _, raw := range doc.NodePools {
		np := &pb.NodePool{}
		if err := protojson.Unmarshal(raw, np); err != nil {
			return err
		}
		_, rev, err := s.GetNodePool(np.Cluster, np.Name)
		if err != nil {
			return err
		}
		if _, err := s.PutNodePoolDesired(np, rev); err != nil {
			return err
		}
	}
	for _, raw := range doc.Machines {
		m := &pb.Machine{}
		if err := protojson.Unmarshal(raw, m); err != nil {
			return err
		}
		_, rev, err := s.GetMachine(m.Cluster, m.TalosEndpoint)
		if err != nil {
			return err
		}
		if _, err := s.PutMachineDesired(m, rev); err != nil {
			return err
		}
	}
	return nil
}
