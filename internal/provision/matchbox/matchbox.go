// Package matchbox is the file-backed Matchbox implementation of the
// provision.Provisioner seam (design/provisioning-plane.md §3). It writes the
// group / profile / generic-config files Matchbox serves, keyed by a host's MAC,
// into Matchbox's data directory — generalizing the hand-run
// netboot/matchbox/groups + scripts/sync-configs.sh setup Medea absorbs
// (PRD §13 #11).
//
// File-backed is the v2 choice (it matches the deployed setup and unit-tests
// cleanly); a gRPC-API impl behind the same seam stays an option
// (provisioning-plane.md §10).
package matchbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bilby91/medea/internal/provision"
)

// Store writes Matchbox group/profile/generic files under a data root.
type Store struct {
	root    string
	httpURL string // Matchbox's externally-reachable base URL (for the talos.config arg)
}

// subdirs of the Matchbox data path.
const (
	groupsDir   = "groups"
	profilesDir = "profiles"
	genericDir  = "generic" // the machine configs Matchbox serves at /generic
)

// New roots a Store at the Matchbox data directory (its -data-path), creating the
// groups/profiles/generic subdirs (0755 — these are served assets; the machine
// config inside is sensitive and written 0600). httpURL is the base URL nodes
// reach Matchbox at (e.g. "http://10.0.0.5:8086"), used to build the
// node's talos.config kernel arg.
func New(root, httpURL string) (*Store, error) {
	if httpURL == "" {
		return nil, fmt.Errorf("matchbox: httpURL required (the node's talos.config points at it)")
	}
	for _, d := range []string{groupsDir, profilesDir, genericDir} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("matchbox: create %s: %w", d, err)
		}
	}
	return &Store{root: root, httpURL: strings.TrimRight(httpURL, "/")}, nil
}

// group is the Matchbox group schema: match a machine (by MAC) to a profile.
type group struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Profile  string            `json:"profile"`
	Selector map[string]string `json:"selector"`
}

// profile is the Matchbox profile schema: boot assets + the generic config id.
// NOTE: the field Matchbox reads is "generic_id" (not "generic_config") — it
// names a file under <data-path>/generic served at /generic for matched machines.
type profile struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Boot      bootSpec `json:"boot"`
	GenericID string   `json:"generic_id"`
}

type bootSpec struct {
	Kernel string   `json:"kernel"`
	Initrd []string `json:"initrd,omitempty"`
	Args   []string `json:"args,omitempty"`
}

// Stage writes the generic machine config (0600), the profile (boot assets), and
// the group (MAC → profile) for a host. Idempotent.
func (s *Store) Stage(_ context.Context, mac string, p provision.Profile, machineConfig []byte) error {
	if mac == "" {
		return fmt.Errorf("matchbox: empty mac")
	}
	key := keyFor(mac)

	// Machine config served to the node — sensitive, 0600.
	if err := os.WriteFile(s.path(genericDir, key), machineConfig, 0o600); err != nil {
		return fmt.Errorf("matchbox: write generic config: %w", err)
	}
	// talos.config tells the booting node where to fetch its machine config —
	// Matchbox's /generic endpoint, matched by the node's MAC/UUID (rendered by
	// Matchbox's iPXE templating per request). Without it the node can't join.
	args := append([]string(nil), p.Args...)
	args = append(args, fmt.Sprintf("talos.config=%s/generic?mac=${net0/mac:hexhyp}&uuid=${uuid}", s.httpURL))
	if err := s.writeJSON(profilesDir, key, profile{
		ID:        key,
		Name:      key,
		Boot:      bootSpec{Kernel: p.Kernel, Initrd: p.Initrd, Args: args},
		GenericID: key,
	}); err != nil {
		return err
	}
	return s.writeJSON(groupsDir, key, group{
		ID:       key,
		Name:     key,
		Profile:  key,
		Selector: map[string]string{"mac": mac},
	})
}

// Unstage removes a host's group, profile, and generic config. Missing files are
// not an error (idempotent release).
func (s *Store) Unstage(_ context.Context, mac string) error {
	key := keyFor(mac)
	for _, f := range []string{
		s.path(groupsDir, key+".json"),
		s.path(profilesDir, key+".json"),
		s.path(genericDir, key),
	} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("matchbox: remove %s: %w", f, err)
		}
	}
	return nil
}

func (s *Store) writeJSON(dir, key string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(dir, key+".json"), raw, 0o644)
}

func (s *Store) path(dir, name string) string { return filepath.Join(s.root, dir, name) }

// keyFor turns a MAC into a filesystem-safe key (Matchbox group/profile id).
func keyFor(mac string) string {
	return strings.NewReplacer(":", "-", "/", "-", " ", "-").Replace(strings.ToLower(mac))
}

// compile-time check that Store satisfies the seam.
var _ provision.Provisioner = (*Store)(nil)
