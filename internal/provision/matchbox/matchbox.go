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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/crunchloop/medea/internal/provision"
)

// Store writes Matchbox group/profile/generic files under a data root.
type Store struct {
	root     string
	httpURL  string // Matchbox's externally-reachable base URL (for the talos.config arg)
	httpHost string // host[:port] of httpURL, so we don't re-mirror our own assets
	httpc    *http.Client
}

// subdirs of the Matchbox data path.
const (
	groupsDir   = "groups"
	profilesDir = "profiles"
	genericDir  = "generic" // the machine configs Matchbox serves at /generic
	assetsDir   = "assets"  // boot assets (kernel/initramfs) Matchbox serves at /assets
)

// New roots a Store at the Matchbox data directory (its -data-path), creating the
// groups/profiles/generic/assets subdirs (0755 — these are served assets; the
// machine config inside is sensitive and written 0600). httpURL is the base URL
// nodes reach Matchbox at (e.g. "http://10.0.0.5:8086"), used to build the node's
// talos.config kernel arg AND the mirrored boot-asset URLs (see Stage).
func New(root, httpURL string) (*Store, error) {
	if httpURL == "" {
		return nil, fmt.Errorf("matchbox: httpURL required (the node's talos.config points at it)")
	}
	u, err := url.Parse(httpURL)
	if err != nil {
		return nil, fmt.Errorf("matchbox: parse httpURL %q: %w", httpURL, err)
	}
	for _, d := range []string{groupsDir, profilesDir, genericDir, assetsDir} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("matchbox: create %s: %w", d, err)
		}
	}
	return &Store{
		root:     root,
		httpURL:  strings.TrimRight(httpURL, "/"),
		httpHost: u.Host,
		// No client Timeout: the initramfs is tens of MB and the caller's context
		// bounds the operation (Stage runs inside the reconciler's timed phase).
		httpc: &http.Client{},
	}, nil
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
func (s *Store) Stage(ctx context.Context, mac string, p provision.Profile, machineConfig []byte) error {
	if mac == "" {
		return fmt.Errorf("matchbox: empty mac")
	}
	key := keyFor(mac)

	// Machine config served to the node — sensitive, 0600.
	if err := os.WriteFile(s.path(genericDir, key), machineConfig, 0o600); err != nil {
		return fmt.Errorf("matchbox: write generic config: %w", err)
	}
	// Mirror the boot assets (kernel/initramfs) through Matchbox and rewrite the
	// profile to point at them over HTTP. The schematic's factory URLs are HTTPS,
	// but iPXE (both in the lab and on the Beelinks) has no TLS — it silently fails
	// on `kernel https://...`. Medea can reach the factory over HTTPS, so it caches
	// the assets locally and serves them from Matchbox over plain HTTP
	// (design/cluster-bootstrap.md §5.1 / provisioning-plane.md; supersedes the
	// hand-run netboot asset fetch).
	kernel, err := s.mirror(ctx, p.Kernel)
	if err != nil {
		return err
	}
	initrd := make([]string, 0, len(p.Initrd))
	for _, i := range p.Initrd {
		li, err := s.mirror(ctx, i)
		if err != nil {
			return err
		}
		initrd = append(initrd, li)
	}
	// talos.config tells the booting node where to fetch its machine config —
	// Matchbox's /generic endpoint, matched by the node's MAC/UUID (rendered by
	// Matchbox's iPXE templating per request). Without it the node can't join.
	args := append([]string(nil), p.Args...)
	args = append(args, fmt.Sprintf("talos.config=%s/generic?mac=${net0/mac:hexhyp}&uuid=${uuid}", s.httpURL))
	if err := s.writeJSON(profilesDir, key, profile{
		ID:        key,
		Name:      key,
		Boot:      bootSpec{Kernel: kernel, Initrd: initrd, Args: args},
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

// mirror ensures the asset at rawURL is available under Matchbox's /assets and
// returns the Matchbox HTTP URL for it. Non-HTTP(S) URLs (already local/relative)
// and URLs already pointing at this Matchbox pass through unchanged. The local
// path mirrors the source URL path, so it is stable per schematic+version+arch and
// cached across hosts and restarts.
func (s *Store) mirror(ctx context.Context, rawURL string) (string, error) {
	if rawURL == "" {
		return "", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("matchbox: parse boot asset %q: %w", rawURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == s.httpHost {
		return rawURL, nil // already local, relative, or served by us
	}
	rel := strings.TrimPrefix(u.Path, "/")
	dest := filepath.Join(s.root, assetsDir, filepath.FromSlash(rel))
	if err := s.fetchIfAbsent(ctx, rawURL, dest); err != nil {
		return "", err
	}
	return s.httpURL + "/" + assetsDir + "/" + rel, nil
}

// fetchIfAbsent downloads srcURL to dest unless a non-empty file is already there
// (the assets are immutable per schematic+version, so presence == cache hit). The
// download is written to a temp file and renamed, so a served asset is always
// complete (an interrupted mirror never leaves a truncated kernel).
func (s *Store) fetchIfAbsent(ctx context.Context, srcURL, dest string) error {
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("matchbox: mkdir for asset: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("matchbox: mirror %s: %w", srcURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("matchbox: mirror %s: status %d", srcURL, resp.StatusCode)
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("matchbox: write asset %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
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
