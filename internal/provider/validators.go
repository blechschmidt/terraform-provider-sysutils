package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

var (
	_ validator.String = absolutePathValidator{}
	_ validator.String = octalModeValidator{}
	_ validator.String = symlinkTargetValidator{}
	_ validator.String = accountNameValidator{}
	_ validator.String = base64Validator{}
	_ validator.String = durationValidator{}
)

// octalModePattern matches a three or four digit octal mode, optionally
// prefixed by a leading zero (e.g. "644", "0644", "4755", "02775").
var octalModePattern = regexp.MustCompile(`^0?[0-7]{3,4}$`)

// validateAbsolutePath reports why p is not an acceptable managed path, or
// returns nil if it is. A path must be absolute, already in the canonical form
// produced by filepath.Clean (no "..", "." or duplicate/trailing separators),
// and must not be the filesystem root.
func validateAbsolutePath(p string) error {
	if err := validateCanonicalPath(p); err != nil {
		return err
	}
	if p == string(filepath.Separator) {
		return fmt.Errorf("path %q must not be the filesystem root", p)
	}
	return nil
}

// validateCanonicalPath reports why p is not an absolute path in the
// canonical form produced by filepath.Clean, or returns nil if it is. Unlike
// validateAbsolutePath it accepts the filesystem root, which is safe to read
// but never to manage.
func validateCanonicalPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be absolute", p)
	}
	if cleaned := filepath.Clean(p); cleaned != p {
		return fmt.Errorf("path %q must be in canonical form; use %q instead", p, cleaned)
	}
	return nil
}

// validateOctalMode reports why s is not an acceptable mode string, or returns
// nil if it is.
func validateOctalMode(s string) error {
	if !octalModePattern.MatchString(s) {
		return fmt.Errorf("mode %q must be a 3 or 4 digit octal string, optionally with a leading zero (e.g. \"0644\", \"755\", \"1777\")", s)
	}
	return nil
}

// validateSymlinkTarget reports why s is not an acceptable symlink target, or
// returns nil if it is. Targets may be relative and need not exist, but must
// be non-empty and free of NUL bytes, which the kernel cannot represent.
func validateSymlinkTarget(s string) error {
	if s == "" {
		return fmt.Errorf("symlink target must not be empty")
	}
	if strings.ContainsRune(s, 0) {
		return fmt.Errorf("symlink target must not contain NUL bytes")
	}
	return nil
}

// absolutePathValidator validates that a string attribute is an absolute,
// cleaned path other than "/". Rejecting the root guards against a
// directory resource ever recursively deleting the whole filesystem. With
// allowRoot set (for read-only data sources) "/" is accepted.
type absolutePathValidator struct {
	allowRoot bool
}

// absolutePath returns a validator.String enforcing validateAbsolutePath.
func absolutePath() validator.String { return absolutePathValidator{} }

// absolutePathOrRoot returns a validator.String enforcing
// validateCanonicalPath, i.e. absolutePath but permitting "/".
func absolutePathOrRoot() validator.String { return absolutePathValidator{allowRoot: true} }

func (v absolutePathValidator) Description(_ context.Context) string {
	if v.allowRoot {
		return "value must be an absolute, cleaned path"
	}
	return "value must be an absolute, cleaned path other than \"/\""
}

func (v absolutePathValidator) MarkdownDescription(ctx context.Context) string {
	if v.allowRoot {
		return v.Description(ctx)
	}
	return "value must be an absolute, cleaned path other than `/`"
}

func (v absolutePathValidator) validate(p string) error {
	if v.allowRoot {
		return validateCanonicalPath(p)
	}
	return validateAbsolutePath(p)
}

func (v absolutePathValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := v.validate(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid path", capitalize(err.Error())+".")
	}
}

// octalModeValidator validates that a string attribute is an octal mode
// matching octalModePattern.
type octalModeValidator struct{}

// octalMode returns a validator.String enforcing validateOctalMode.
func octalMode() validator.String { return octalModeValidator{} }

func (v octalModeValidator) Description(_ context.Context) string {
	return "value must be a 3 or 4 digit octal mode, optionally with a leading zero"
}

func (v octalModeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v octalModeValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateOctalMode(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid mode", capitalize(err.Error())+".")
	}
}

// symlinkTargetValidator validates that a string attribute is a usable
// symlink target according to validateSymlinkTarget.
type symlinkTargetValidator struct{}

// symlinkTarget returns a validator.String enforcing validateSymlinkTarget.
func symlinkTarget() validator.String { return symlinkTargetValidator{} }

func (v symlinkTargetValidator) Description(_ context.Context) string {
	return "value must be a non-empty path without NUL bytes"
}

func (v symlinkTargetValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v symlinkTargetValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateSymlinkTarget(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid target", capitalize(err.Error())+".")
	}
}

// capitalize upper-cases the first byte of an ASCII message so error strings
// (lower-case by Go convention) read as sentences in diagnostics.
func capitalize(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// accountNamePattern matches user and group names accepted by the provider:
// portable characters, not starting with "-" (which shadow-utils would parse
// as an option), optionally ending in "$" for Samba machine accounts.
var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\$?$`)

// validateAccountName reports why s is not an acceptable user or group name,
// or returns nil if it is. Beyond accountNamePattern, names are limited to 32
// bytes and must not be purely numeric, since they would be ambiguous with
// numeric IDs.
func validateAccountName(s string) error {
	if s == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(s) > 32 {
		return fmt.Errorf("name %q must be at most 32 characters long", s)
	}
	if !accountNamePattern.MatchString(s) {
		return fmt.Errorf("name %q must consist of letters, digits, \"_\", \".\" and \"-\", must not start with \"-\" or \".\", and may only end in \"$\"", s)
	}
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return fmt.Errorf("name %q must not be purely numeric", s)
	}
	return nil
}

// accountNameValidator validates that a string attribute is a user or group
// name according to validateAccountName.
type accountNameValidator struct{}

// accountName returns a validator.String enforcing validateAccountName.
func accountName() validator.String { return accountNameValidator{} }

func (v accountNameValidator) Description(_ context.Context) string {
	return "value must be a valid user or group name"
}

func (v accountNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v accountNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateAccountName(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid name", capitalize(err.Error())+".")
	}
}

// base64Validator validates that a string attribute is standard (RFC 4648,
// padded) base64, as produced by Terraform's base64encode and filebase64.
type base64Validator struct{}

// base64String returns a validator.String enforcing standard base64.
func base64String() validator.String { return base64Validator{} }

func (v base64Validator) Description(_ context.Context) string {
	return "value must be valid standard base64"
}

func (v base64Validator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v base64Validator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := base64.StdEncoding.DecodeString(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid base64", fmt.Sprintf("Value is not valid standard base64: %s.", err))
	}
}

// validateDuration reports why s is not a positive Go duration string, or
// returns nil if it is.
func validateDuration(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a valid duration; use a number with a unit suffix such as \"30s\", \"5m\" or \"1h30m\"", s)
	}
	if d <= 0 {
		return fmt.Errorf("duration %q must be greater than zero", s)
	}
	return nil
}

// durationValidator validates that a string attribute is a positive Go
// duration as accepted by time.ParseDuration (e.g. "90s", "5m", "1h30m").
type durationValidator struct{}

// positiveDuration returns a validator.String enforcing a positive Go duration.
func positiveDuration() validator.String { return durationValidator{} }

func (v durationValidator) Description(_ context.Context) string {
	return "value must be a positive Go duration such as \"30s\", \"5m\" or \"1h30m\""
}

func (v durationValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v durationValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateDuration(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid duration", capitalize(err.Error())+".")
	}
}

// unitNameValidator validates that a string attribute is a systemd unit name
// according to validateUnitName.
type unitNameValidator struct{}

// unitName returns a validator.String enforcing validateUnitName.
func unitName() validator.String { return unitNameValidator{} }

func (v unitNameValidator) Description(_ context.Context) string {
	return "value must be a systemd unit name such as \"app.service\""
}

func (v unitNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v unitNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := validateUnitName(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid unit name", capitalize(err.Error())+".")
	}
}
