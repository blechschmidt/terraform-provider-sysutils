package provider

// Private-state bookkeeping for the write-only content_wo of sysutils_file.
//
// Terraform keeps no trace of a write-only value, so the provider keeps its
// own record of what it wrote, to detect a file changed outside Terraform.
// A plain checksum of the content would let anyone who can read the state
// test guesses of a low-entropy secret, such as a password, at the speed of
// SHA-256 or MD5; for the same reason content_sha256 and content_md5 stay
// null with content_wo. The record instead holds an argon2id hash of the
// content's SHA-256 digest under a random salt, so that every guess costs
// argon2id's memory-hard work.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/crypto/argon2"
)

const (
	// fileContentWOPrivateKey is the private state key of the
	// contentWORecord, as JSON. Its presence also marks a resource whose
	// content must never be read into the state.
	fileContentWOPrivateKey = "content_wo"
	// fileContentWOLegacyPrivateKey is where development builds before
	// v1.3.0 kept the plain hex SHA-256 of the written content. It is
	// removed whenever the private state is written.
	fileContentWOLegacyPrivateKey = "content_wo_sha256"

	// argon2id parameters: OWASP's recommended minimum (19 MiB, two
	// passes, one thread), about 20 ms per hash.
	contentWOArgonTime    = 2
	contentWOArgonMemory  = 19 * 1024 // KiB
	contentWOArgonThreads = 1
	contentWOSaltSize     = 16
	contentWOHashSize     = 32
)

// contentWORecord describes the content last written from content_wo.
type contentWORecord struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"argon2id"`
	// Drift is set by refresh when the file no longer has the recorded
	// content, so that the plan rewrites it.
	Drift bool `json:"drift,omitempty"`
}

func contentWOFingerprint(digest, salt []byte) []byte {
	return argon2.IDKey(digest, salt, contentWOArgonTime, contentWOArgonMemory, contentWOArgonThreads, contentWOHashSize)
}

// newContentWORecord returns the record for written content whose SHA-256
// checksum is sha256Hex, under a new random salt.
func newContentWORecord(sha256Hex string) (*contentWORecord, error) {
	digest, err := hex.DecodeString(sha256Hex)
	if err != nil {
		return nil, fmt.Errorf("invalid checksum: %w", err)
	}
	salt := make([]byte, contentWOSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}
	return &contentWORecord{Salt: salt, Hash: contentWOFingerprint(digest, salt)}, nil
}

// matches reports whether content whose SHA-256 checksum is sha256Hex is
// the recorded content. A record without a hash matches nothing.
func (r *contentWORecord) matches(sha256Hex string) bool {
	if len(r.Salt) == 0 || len(r.Hash) != contentWOHashSize {
		return false
	}
	digest, err := hex.DecodeString(sha256Hex)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(contentWOFingerprint(digest, r.Salt), r.Hash) == 1
}

// loadContentWORecord returns the record in p, or nil if there is none. A
// legacy record, which held a plain checksum, is returned as drift, so that
// the next apply rewrites the file and stores a new record in its place.
func loadContentWORecord(ctx context.Context, p privateState) (*contentWORecord, diag.Diagnostics) {
	raw, diags := p.GetKey(ctx, fileContentWOPrivateKey)
	if diags.HasError() {
		return nil, diags
	}
	if len(raw) == 0 {
		legacy, d := p.GetKey(ctx, fileContentWOLegacyPrivateKey)
		diags.Append(d...)
		if len(legacy) != 0 {
			return &contentWORecord{Drift: true}, diags
		}
		return nil, diags
	}
	var rec contentWORecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		diags.AddError("Invalid private state", fmt.Sprintf("Decoding %q: %s.", fileContentWOPrivateKey, err))
		return nil, diags
	}
	return &rec, diags
}

// storeContentWORecord stores rec in p, or removes the record if rec is
// nil. Either way, a legacy record is removed.
func storeContentWORecord(ctx context.Context, p privateStateWriter, rec *contentWORecord) diag.Diagnostics {
	diags := p.SetKey(ctx, fileContentWOLegacyPrivateKey, nil)
	if rec == nil {
		return append(diags, p.SetKey(ctx, fileContentWOPrivateKey, nil)...)
	}
	return append(diags, setPrivateJSON(ctx, p, fileContentWOPrivateKey, rec)...)
}

// contentWONeedsWrite reports whether content_wo must be written to the file
// of the resource whose refreshed prior state is prior, given the planned
// content_wo_version and the record from private state (nil if none). It
// must be: when the version changes (or is not known yet), when nothing was
// recorded (as after import, or when switching from another content
// attribute), and when refresh found the file changed. A changed content_wo
// alone is not detected; Terraform keeps no trace of it.
func contentWONeedsWrite(prior *fileModel, version types.Int64, rec *contentWORecord) bool {
	return version.IsUnknown() ||
		!version.Equal(prior.ContentWOVersion) ||
		rec == nil || rec.Drift
}

// usesNoContentAttribute reports whether none of the stored content
// attributes of m is set, as for a resource that uses content_wo.
func usesNoContentAttribute(m *fileModel) bool {
	return m.Content.IsNull() && m.SensitiveContent.IsNull() && m.ContentBase64.IsNull() && m.Source.IsNull()
}
