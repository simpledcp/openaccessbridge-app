package update

// The appliance-side service: what an operator can actually do with an update
// bundle on this image.
//
// §9.4's update path is "a signed bundle applied from local media". On this
// image the APPLY half cannot exist — no A/B partitions, no bootloader
// integration, no TPM (BLOCKERS B8) — and the honest response to that is not to
// simulate it. What the appliance can do completely and truthfully is VERIFY:
// read a bundle from the media directory, check it against the keys it holds,
// and tell an operator precisely what is wrong with it or that it is good.
//
// That is worth having on its own. The failure this prevents is a booth
// discovering at 19:45 that the stick they were sent is the wrong build, is for
// another tier, or was never signed by anyone this appliance trusts.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrBadBundleName is returned for a name that is not a plain filename.
var ErrBadBundleName = errors.New("update: a bundle name must be a plain filename on the update media")

// Service verifies bundles and reports the A/B state.
type Service struct {
	trust    *TrustStore
	mediaDir string
	platform Platform
	current  func() Current
	status   func() SlotStatus
	now      func() time.Time
}

// Options configures a Service.
type Options struct {
	Trust *TrustStore
	// MediaDir is the directory bundles are read from — a mount point for the
	// removable media §9.4 describes. Empty means no media is configured and
	// nothing can be read.
	MediaDir string
	// Platform is the A/B implementation. Nil means UnsupportedPlatform, which is
	// what this image ships.
	Platform Platform
	Current  func() Current
	Status   func() SlotStatus
	Now      func() time.Time
}

// NewService builds the service.
func NewService(opts Options) *Service {
	s := &Service{
		trust: opts.Trust, mediaDir: opts.MediaDir,
		platform: opts.Platform, current: opts.Current, status: opts.Status, now: opts.Now,
	}
	if s.platform == nil {
		s.platform = UnsupportedPlatform{}
	}
	if s.current == nil {
		s.current = func() Current { return Current{} }
	}
	if s.status == nil {
		s.status = func() SlotStatus { return SlotStatus{} }
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Report is the operator-facing update state.
type Report struct {
	// TrustedKeys is how many update-signing keys this appliance holds. Zero
	// means no update can be verified at all, which is a fact worth seeing before
	// a bundle arrives rather than while one is being installed.
	TrustedKeys int `json:"trusted_keys"`
	// MediaDir is where bundles are read from, empty when none is configured.
	MediaDir string `json:"media_dir,omitempty"`
	// Bundles are the candidate files on that media.
	Bundles []string `json:"bundles,omitempty"`
	// MediaReadable says whether the media directory could be read at all. An
	// unplugged stick, a bad mount or a permissions problem is NOT the same fact
	// as "there are no bundles here", and an operator who has just inserted media
	// needs to be able to tell them apart.
	//
	// It is false when no directory is CONFIGURED too — a third state — so
	// MediaError always says which of the three this is rather than leaving a
	// bare false to be read as "the media is broken".
	MediaReadable bool `json:"media_readable"`
	// MediaError is why it could not be read, or that none is configured.
	MediaError string `json:"media_error,omitempty"`
	// Slots is the A/B state.
	Slots SlotStatus `json:"slots"`
	// ABSupported: this machine can actually stage an image into a second slot
	// and boot it. FALSE on this image, and the field exists so that a verified
	// bundle can never read as an installable one.
	ABSupported bool `json:"ab_supported"`
	// ABNote says what is missing when it is false.
	ABNote string `json:"ab_note,omitempty"`
}

// Status reports the update state.
func (s *Service) Status() Report {
	rep := Report{
		TrustedKeys: s.trust.Len(),
		MediaDir:    s.mediaDir,
		Slots:       s.status(),
	}
	_, ok := s.platform.Slots()
	rep.ABSupported = ok
	if !ok {
		rep.ABNote = "This appliance has no A/B slots: a verified bundle can be checked here but not " +
			"installed, and nothing can be rolled back. Installing updates needs an image built with " +
			"two slots and a bootloader that honours them (BLOCKERS B8)."
	}
	if s.mediaDir == "" {
		rep.MediaError = "no update media directory is configured, so no bundle can be read"
	} else {
		entries, err := os.ReadDir(s.mediaDir)
		if err != nil {
			rep.MediaError = err.Error()
		} else {
			rep.MediaReadable = true
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".oabupd") {
					rep.Bundles = append(rep.Bundles, e.Name())
				}
			}
		}
	}
	return rep
}

// VerifyResult is the outcome of checking one bundle.
type VerifyResult struct {
	// Name is the file checked.
	Name string `json:"name"`
	// OK is true only when every check passed.
	OK bool `json:"ok"`
	// Reason is why it failed, in the words an operator needs.
	Reason string `json:"reason,omitempty"`
	// Manifest is what the bundle claims. It is reported even for a FAILED
	// verification — an operator needs to see which build the wrong stick holds —
	// and every surface that renders it must say whether OK is true beside it.
	Manifest *Manifest `json:"manifest,omitempty"`
	// KeyID names the key that verified it, when one did.
	KeyID string `json:"key_id,omitempty"`
	// Installable is separate from OK: a bundle can be perfectly valid and still
	// not installable on an appliance with no A/B slots. Collapsing the two would
	// let "this bundle is good" read as "this update can be applied".
	Installable bool `json:"installable"`
}

// VerifyFile checks one bundle on the update media.
func (s *Service) VerifyFile(name string) (VerifyResult, error) {
	if s.mediaDir == "" {
		return VerifyResult{}, errors.New("update: no update media directory is configured")
	}
	// A plain filename only. The name comes from an operator surface, and a path
	// is not something this should ever be asked to interpret.
	if name != filepath.Base(name) || strings.TrimSpace(name) == "" || strings.Contains(name, string(filepath.Separator)) {
		return VerifyResult{}, fmt.Errorf("%w: %q", ErrBadBundleName, name)
	}
	path := filepath.Join(s.mediaDir, name)

	info, err := os.Stat(path)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("update: cannot read %q from the update media: %w", name, err)
	}
	if info.Size() > MaxPayloadBytes+MaxManifestBytes+4096 {
		return VerifyResult{Name: name, Reason: ErrTooLarge.Error()}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("update: cannot read %q: %w", name, err)
	}

	res := VerifyResult{Name: name}
	b, err := Parse(raw)
	if err != nil {
		res.Reason = err.Error()
		return res, nil
	}
	m := b.Manifest
	res.Manifest = &m

	v, err := Verify(b, s.trust, s.current())
	if err != nil {
		res.Reason = err.Error()
		return res, nil
	}
	res.OK = true
	res.KeyID = v.KeyID
	_, abOK := s.platform.Slots()
	res.Installable = abOK
	return res, nil
}
