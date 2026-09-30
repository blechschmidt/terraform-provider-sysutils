package provider

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Exposed as a checksum for interoperability, not for security.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*fileResource)(nil)
	_ resource.ResourceWithImportState      = (*fileResource)(nil)
	_ resource.ResourceWithConfigValidators = (*fileResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*fileResource)(nil)
	_ resource.ResourceWithConfigure        = (*fileResource)(nil)
)

const defaultFileMode = "0644"

func NewFileResource() resource.Resource { return &fileResource{} }

type fileResource struct{ rootedResource }

type fileModel struct {
	Path             types.String `tfsdk:"path"`
	Content          types.String `tfsdk:"content"`
	SensitiveContent types.String `tfsdk:"sensitive_content"`
	ContentWO        types.String `tfsdk:"content_wo"`
	ContentWOVersion types.Int64  `tfsdk:"content_wo_version"`
	ContentBase64    types.String `tfsdk:"content_base64"`
	Source           types.String `tfsdk:"source"`
	ContentSHA256    types.String `tfsdk:"content_sha256"`
	ContentMD5       types.String `tfsdk:"content_md5"`
	Mode             types.String `tfsdk:"mode"`
	Owner            types.String `tfsdk:"owner"`
	Group            types.String `tfsdk:"group"`
	ID               types.String `tfsdk:"id"`
}

func (r *fileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file"
}

func (r *fileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Writes a file at `path` whose contents come from exactly one of `content` (UTF-8 text), `sensitive_content` (UTF-8 text hidden from plans), `content_wo` (UTF-8 text never stored in state, Terraform 1.11 and later), `content_base64` (binary data) or `source` (a local file to copy). " +
			"On create the provider also sets the requested `mode`, and optionally `owner` / `group` (which require privileges). " +
			"On update, content, mode, and ownership are reconciled in-place. On destroy, the file is removed.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path where the file should be written. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"content": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "File contents as UTF-8 text. Exactly one of `content`, `sensitive_content`, `content_wo`, `content_base64` and `source` must be set. " +
					"If the file is changed outside Terraform, refresh records its actual text here, so the plan shows a line-by-line diff of the drift.",
			},
			"sensitive_content": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Like `content`, for secrets: the value is marked sensitive, so plans show only `(sensitive value)` for it and a change is visible only through `content_sha256` and `content_md5`. " +
					"Exactly one of `content`, `sensitive_content`, `content_wo`, `content_base64` and `source` must be set. " +
					"The value is still stored in the state in plain text; use `content_wo` to keep it out of the state.",
			},
			"content_wo": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
				MarkdownDescription: "File contents as UTF-8 text, as a write-only argument: " +
					"the value is never stored in the plan or the state, so it may come from an ephemeral resource or ephemeral variable. Requires Terraform 1.11 or later. " +
					"Exactly one of `content`, `sensitive_content`, `content_wo`, `content_base64` and `source` must be set. " +
					"Terraform does not remember the value, so a changed value is not detected: the file is written on create, when `content_wo_version` changes, and when the file on disk no longer matches what the provider last wrote. " +
					"For that comparison, the resource's private state keeps a salted argon2id hash of the written content. " +
					"`content_sha256` and `content_md5` are always null with `content_wo`, as a checksum of a short secret would let anyone who can read the state check guesses; " +
					"use `content_wo_version` to make other resources react to a new value.",
			},
			"content_wo_version": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Any number; change it to write the current value of `content_wo` to the file. Requires `content_wo`. " +
					"Changing it plans an in-place update; the value itself is stored in the state.",
				Validators: []validator.Int64{int64validator.AlsoRequires(path.MatchRoot("content_wo"))},
			},
			"content_base64": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "File contents as standard (padded) base64, for binary data. " +
					"Use `filebase64()` or `base64encode()` to produce it. " +
					"Exactly one of `content`, `sensitive_content`, `content_wo`, `content_base64` and `source` must be set. " +
					"If the file is changed outside Terraform, refresh records the base64 encoding of its actual contents here, so the plan shows the drift.",
				Validators: []validator.String{base64String()},
			},
			"source": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Path to a local file to copy to `path`. Exactly one of `content`, `sensitive_content`, `content_wo`, `content_base64` and `source` must be set. " +
					"Relative paths are resolved against Terraform's working directory; prefer `${path.module}/...`. The provider's `root_dir` does not apply to `source`. " +
					"Its contents are not stored in the state, so plans show changes only through `content_sha256` and `content_md5`. " +
					"The source is read and hashed during every plan, so a change to its contents plans an update even if the configuration is unchanged. " +
					"Symlinks are followed for `source`, but it must resolve to a regular file. " +
					"If the source does not exist at plan time (for example because another resource creates it in the same apply), the checksums are unknown until apply.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"content_sha256": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded SHA-256 checksum of the file contents. " +
					"Computed from the configuration during plan (from the source file, for `source`), so it is known before apply and other resources can depend on it to react to content changes. " +
					"Always null with `content_wo`.",
			},
			"content_md5": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded MD5 checksum of the file contents. " +
					"Provided for interoperability; use `content_sha256` where integrity matters. " +
					"Like `content_sha256`, it is known at plan time, and always null with `content_wo`.",
			},
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultFileMode),
				MarkdownDescription: "Octal mode with 3 or 4 digits, optionally with a leading zero, such as `\"0644\"`, `\"600\"` or `\"4755\"`. " +
					"The mode is applied with an explicit `chmod`, so the process umask does not affect it. Defaults to `\"0644\"`.",
				Validators: []validator.String{octalMode()},
			},
			"owner": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Username or numeric UID that should own the file. Requires privileges to change. " +
					"If unset, the owner assigned at creation (normally the user running Terraform) is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"group": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name or numeric GID of the file. Requires privileges to change. " +
					"If unset, the group assigned at creation is kept.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *fileResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(
			path.MatchRoot("content"),
			path.MatchRoot("sensitive_content"),
			path.MatchRoot("content_wo"),
			path.MatchRoot("content_base64"),
			path.MatchRoot("source"),
		),
	}
}

// ModifyPlan sets content_sha256 and content_md5 to the checksums of the
// desired content. Refresh records the checksums of the file on disk, so a
// mismatch -- whether from out-of-band edits or from a changed source file --
// shows up as a planned update even when no configured attribute changed.
func (r *fileResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var config fileModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	sha, md := types.StringUnknown(), types.StringUnknown()
	if !config.ContentWO.IsNull() {
		// The value of content_wo is not planned: an ephemeral value may
		// differ at apply. Its checksums are never stored, as they would
		// reveal a low-entropy secret; they are planned as unknown (and
		// become null) when an existing file must be rewritten, so that
		// drift found by refresh plans an update.
		var prior *fileModel
		if !req.State.Raw.IsNull() {
			prior = &fileModel{}
			resp.Diagnostics.Append(req.State.Get(ctx, prior)...)
			// A new path replaces the resource, which writes the file.
			if !prior.Path.Equal(config.Path) {
				prior = nil
			}
		}
		rec, diags := loadContentWORecord(ctx, req.Private)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		if prior == nil || !contentWONeedsWrite(prior, config.ContentWOVersion, rec) {
			sha, md = types.StringNull(), types.StringNull()
		}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_sha256"), sha)...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_md5"), md)...)
		return
	}
	sums, known, err := desiredChecksums(&config)
	if err != nil {
		resp.Diagnostics.AddAttributeError(contentAttribute(&config), "Unable to read file contents", capitalize(err.Error())+".")
		return
	}
	if known {
		sha, md = types.StringValue(sums.sha256), types.StringValue(sums.md5)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_sha256"), sha)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("content_md5"), md)...)
}

func (r *fileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		resp.Diagnostics.AddError("Creating parent directory", err.Error())
		return
	}
	// content_wo is null in the plan; its value is only in the config.
	var config fileModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	writeOnly := !config.ContentWO.IsNull()

	// Owner and group are unknown when not configured; ValueString returns ""
	// for unknown values, meaning "leave unchanged".
	var written checksums
	if writeOnly {
		written, err = writeFile(target, strings.NewReader(config.ContentWO.ValueString()), mode, plan.Owner.ValueString(), plan.Group.ValueString())
	} else {
		written, err = writeDesiredContent(target, &plan, mode, plan.Owner.ValueString(), plan.Group.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
		return
	}
	planned := plan.ContentSHA256
	var rec *contentWORecord
	if writeOnly {
		if rec, err = newContentWORecord(written.sha256); err != nil {
			resp.Diagnostics.AddError("Recording written content", capitalize(err.Error())+".")
			return
		}
	}

	found, diags := readFile(target, &plan, rec)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Writing file",
			fmt.Sprintf("File %q disappeared immediately after creation.", target))
		return
	}
	if writeOnly {
		resp.Diagnostics.Append(storeContentWORecord(ctx, resp.Private, rec)...)
	}
	// The file exists either way; record it so a failure below taints it
	// rather than orphaning it.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if !writeOnly {
		resp.Diagnostics.Append(checkWrittenChecksum(&plan, planned, written)...)
	}
}

func (r *fileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A resource that uses content_wo has a record of what was written in
	// its private state; its content must never be read into the state.
	rec, diags := loadContentWORecord(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Without a record, a resource that has been read before (unlike one
	// being imported, whose mode is not known yet) and stores no content
	// uses content_wo as well; its file is rewritten on the next apply.
	if rec == nil && usesNoContentAttribute(&state) && !state.Mode.IsNull() {
		rec = &contentWORecord{Drift: true}
	}
	found, diags := readFile(target, &state, rec)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if rec != nil {
		resp.Diagnostics.Append(storeContentWORecord(ctx, resp.Private, rec)...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *fileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state fileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, diags := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return
	}

	// Only chown what actually changed, so that an unrelated update does not
	// require the privileges needed to (re)assign ownership.
	var owner, group string
	if !plan.Owner.IsUnknown() && !plan.Owner.Equal(state.Owner) {
		owner = plan.Owner.ValueString()
	}
	if !plan.Group.IsUnknown() && !plan.Group.Equal(state.Group) {
		group = plan.Group.ValueString()
	}
	var config fileModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	writeOnly := !config.ContentWO.IsNull()

	var written checksums
	var rec *contentWORecord
	if writeOnly {
		var diags diag.Diagnostics
		rec, diags = loadContentWORecord(ctx, req.Private)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		// The same decision as in ModifyPlan: without a new version or
		// drift, only mode and ownership are applied.
		if contentWONeedsWrite(&state, plan.ContentWOVersion, rec) {
			written, err = writeFile(target, strings.NewReader(config.ContentWO.ValueString()), mode, owner, group)
			if err == nil {
				rec, err = newContentWORecord(written.sha256)
			}
		} else {
			err = applyFileMetadata(target, mode, owner, group)
		}
	} else {
		written, err = writeDesiredContent(target, &plan, mode, owner, group)
	}
	if err != nil {
		resp.Diagnostics.AddError("Writing file", err.Error())
		return
	}
	planned := plan.ContentSHA256

	found, diags := readFile(target, &plan, rec)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Writing file",
			fmt.Sprintf("File %q disappeared during update.", target))
		return
	}
	// A nil rec removes the record when switching away from content_wo.
	resp.Diagnostics.Append(storeContentWORecord(ctx, resp.Private, rec)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if !writeOnly {
		resp.Diagnostics.Append(checkWrittenChecksum(&plan, planned, written)...)
	}
}

func (r *fileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// State is not validated by the schema; never act on an invalid path.
	if err := validateAbsolutePath(state.Path.ValueString()); err != nil {
		resp.Diagnostics.AddError("Refusing to remove file", capitalize(err.Error())+".")
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// unlink never follows symlinks and never removes directories, unlike
	// os.Remove, which falls back to rmdir.
	if err := syscall.Unlink(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing file", explainImmutable(&fs.PathError{Op: "unlink", Path: target, Err: err}, filepath.Dir(target)).Error())
		return
	}
}

func (r *fileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the file: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// writeFile writes content to target and applies mode and ownership. Empty
// owner or group values leave the corresponding attribute unchanged. It
// returns the checksums of the bytes written.
//
// A symlink or other non-regular file at target is refused. The file is
// opened with O_NOFOLLOW and all changes go through that descriptor, so it
// cannot be swapped for a symlink between the check and the write. Ownership
// and mode are applied before the new content is written, so content never
// becomes visible under a more permissive mode or previous owner.
func writeFile(target string, content io.Reader, mode fs.FileMode, owner, group string) (checksums, error) {
	sums, err := writeFileTo(target, content, mode, owner, group)
	return sums, explainImmutable(err, target, filepath.Dir(target))
}

func writeFileTo(target string, content io.Reader, mode fs.FileMode, owner, group string) (checksums, error) {
	if info, err := os.Lstat(target); err == nil {
		if err := checkRegularFile(target, info); err != nil {
			return checksums{}, err
		}
	}
	// A newly created file starts out private; its final mode is set below.
	f, err := openNoFollow(target, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return checksums{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return checksums{}, err
	}
	if err := checkRegularFile(target, info); err != nil {
		return checksums{}, err
	}
	if err := setOwnershipAndMode(f, owner, group, mode); err != nil {
		return checksums{}, err
	}
	if err := f.Truncate(0); err != nil {
		return checksums{}, err
	}
	h := newChecksummer()
	if _, err := io.Copy(io.MultiWriter(f, h), content); err != nil {
		return checksums{}, err
	}
	if err := f.Close(); err != nil {
		return checksums{}, err
	}
	return h.sums(), nil
}

// applyFileMetadata applies mode and ownership to the existing regular file
// target without touching its content, with the same guarantees as
// writeFile.
func applyFileMetadata(target string, mode fs.FileMode, owner, group string) error {
	f, err := openNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		return explainImmutable(err, target)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := checkRegularFile(target, info); err != nil {
		return err
	}
	return setOwnershipAndMode(f, owner, group, mode)
}

// writeDesiredContent writes the content configured in m -- content,
// content_base64 or source -- to target via writeFile.
func writeDesiredContent(target string, m *fileModel, mode fs.FileMode, owner, group string) (checksums, error) {
	switch {
	case !m.Content.IsNull():
		return writeFile(target, strings.NewReader(m.Content.ValueString()), mode, owner, group)
	case !m.SensitiveContent.IsNull():
		return writeFile(target, strings.NewReader(m.SensitiveContent.ValueString()), mode, owner, group)
	case !m.ContentBase64.IsNull():
		data, err := base64.StdEncoding.DecodeString(m.ContentBase64.ValueString())
		if err != nil {
			return checksums{}, fmt.Errorf("decoding content_base64: %w", err)
		}
		return writeFile(target, bytes.NewReader(data), mode, owner, group)
	case !m.Source.IsNull():
		src, err := openSource(m.Source.ValueString())
		if err != nil {
			return checksums{}, err
		}
		defer func() { _ = src.Close() }()
		return writeFile(target, src, mode, owner, group)
	default:
		return checksums{}, errors.New("one of content, sensitive_content, content_base64 or source must be set")
	}
}

// checkWrittenChecksum reports an error if the content written differs from
// what was planned, which happens when the source file changes between plan
// and apply. Unknown planned checksums (e.g. a source that did not exist yet
// at plan time) are accepted.
func checkWrittenChecksum(m *fileModel, planned types.String, written checksums) diag.Diagnostics {
	var diags diag.Diagnostics
	if planned.IsUnknown() || planned.IsNull() || planned.ValueString() == written.sha256 {
		return diags
	}
	diags.AddAttributeError(contentAttribute(m), "Content changed during apply",
		fmt.Sprintf("The content written to %q has SHA-256 %s, but the plan expected %s. "+
			"The source file was probably modified between plan and apply; run terraform apply again.",
			m.Path.ValueString(), written.sha256, planned.ValueString()))
	return diags
}

// checksums holds hex-encoded digests of a file's contents.
type checksums struct {
	sha256 string
	md5    string
}

// checksummer is an io.Writer that computes checksums of everything written
// to it.
type checksummer struct {
	sha, md hash.Hash
}

func newChecksummer() *checksummer {
	return &checksummer{sha: sha256.New(), md: md5.New()} //nolint:gosec // See import.
}

func (c *checksummer) Write(p []byte) (int, error) {
	_, _ = c.sha.Write(p) // hash.Hash writes never fail.
	_, _ = c.md.Write(p)
	return len(p), nil
}

func (c *checksummer) sums() checksums {
	return checksums{sha256: hex.EncodeToString(c.sha.Sum(nil)), md5: hex.EncodeToString(c.md.Sum(nil))}
}

// checksumReader returns the checksums of everything read from r.
func checksumReader(r io.Reader) (checksums, error) {
	h := newChecksummer()
	if _, err := io.Copy(h, r); err != nil {
		return checksums{}, err
	}
	return h.sums(), nil
}

// openSource opens the source file p for reading. Unlike the managed path,
// p may be (or traverse) a symlink, but it must resolve to a regular file:
// the open is non-blocking so a FIFO cannot hang the provider, and devices
// or directories are refused after the open.
func openSource(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening source: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("opening source: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("source %q is not a regular file", p)
	}
	return f, nil
}

// desiredChecksums returns the checksums of the content configured in m.
// known is false if the content cannot be determined yet: an attribute is
// unknown, or the source file does not exist (it may be created during the
// same apply).
func desiredChecksums(m *fileModel) (sums checksums, known bool, err error) {
	switch {
	case m.Content.IsUnknown() || m.SensitiveContent.IsUnknown() || m.ContentBase64.IsUnknown() || m.Source.IsUnknown():
		return checksums{}, false, nil
	case !m.Content.IsNull():
		sums, err = checksumReader(strings.NewReader(m.Content.ValueString()))
		return sums, err == nil, err
	case !m.SensitiveContent.IsNull():
		sums, err = checksumReader(strings.NewReader(m.SensitiveContent.ValueString()))
		return sums, err == nil, err
	case !m.ContentBase64.IsNull():
		data, err := base64.StdEncoding.DecodeString(m.ContentBase64.ValueString())
		if err != nil {
			return checksums{}, false, fmt.Errorf("decoding content_base64: %w", err)
		}
		sums, err = checksumReader(bytes.NewReader(data))
		return sums, err == nil, err
	case !m.Source.IsNull():
		src, err := openSource(m.Source.ValueString())
		if errors.Is(err, fs.ErrNotExist) {
			return checksums{}, false, nil
		}
		if err != nil {
			return checksums{}, false, err
		}
		defer func() { _ = src.Close() }()
		sums, err = checksumReader(src)
		if err != nil {
			return checksums{}, false, fmt.Errorf("reading source: %w", err)
		}
		return sums, true, nil
	default:
		return checksums{}, false, nil
	}
}

// contentAttribute returns the path of whichever content attribute is set in
// m, for attaching diagnostics.
func contentAttribute(m *fileModel) path.Path {
	switch {
	case !m.SensitiveContent.IsNull():
		return path.Root("sensitive_content")
	case !m.ContentBase64.IsNull():
		return path.Root("content_base64")
	case !m.Source.IsNull():
		return path.Root("source")
	default:
		return path.Root("content")
	}
}

// checkRegularFile returns a descriptive error unless info describes a
// regular file.
func checkRegularFile(target string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return symlinkRefusedError(target)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path %q exists but is not a regular file", target)
	}
	return nil
}

// refreshContent records the file content read from disk in whichever
// content attribute m uses; see readFile.
func refreshContent(m *fileModel, content []byte) {
	// Terraform strings must be valid UTF-8.
	text := utf8.Valid(content)
	switch {
	case !m.SensitiveContent.IsNull():
		if text {
			m.SensitiveContent = types.StringValue(string(content))
		}
	case !m.ContentBase64.IsNull():
		prior, err := base64.StdEncoding.DecodeString(m.ContentBase64.ValueString())
		if err != nil || !bytes.Equal(prior, content) {
			m.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(content))
		}
	case text:
		m.Content = types.StringValue(string(content))
	case m.Content.IsNull():
		m.ContentBase64 = types.StringValue(base64.StdEncoding.EncodeToString(content))
	}
}

// readFile refreshes m's checksums, mode, owner, group and id from the file at
// target. Configured values that are equivalent to the actual ones (e.g. "644"
// vs "0644", or a numeric UID vs its username) are preserved to avoid spurious
// diffs. It reports found=false if target does not exist.
//
// How content is refreshed depends on which content attribute m uses:
//   - content: set to the file's text, so drift shows as a readable diff. If
//     the file is no longer valid UTF-8, the prior value is kept and drift is
//     detected through content_sha256 instead.
//   - sensitive_content: the same, but Terraform hides the value, so drift
//     is visible only through content_sha256 and content_md5.
//   - content_base64: set to the base64 encoding of the file, unless the
//     prior value already decodes to the same bytes (it need not be the
//     canonical encoding, and rewriting it would plan a spurious update).
//   - source: left untouched; the source's contents are never stored, and
//     drift is detected by comparing content_sha256 with the checksum
//     computed during plan.
//   - content_wo (wo is not nil): left untouched as well, and the checksums
//     are null, as they would reveal a low-entropy secret. wo.Drift is set
//     if the file no longer has the content wo records.
//   - none (after import): content if the file is UTF-8 text, otherwise
//     content_base64.
func readFile(target string, m *fileModel, wo *contentWORecord) (found bool, diags diag.Diagnostics) {
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, diags
		}
		diags.AddError("Stat failed", err.Error())
		return false, diags
	}
	if err := checkRegularFile(target, info); err != nil {
		diags.AddAttributeError(path.Root("path"), "Not a regular file", capitalize(err.Error())+".")
		return true, diags
	}

	// Open without following symlinks and re-check the type on the
	// descriptor, so a path swapped after Lstat (e.g. for a FIFO that would
	// block, or a symlink to a sensitive file) is never read.
	f, err := openNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Removed between Lstat and Open.
			return false, diags
		}
		diags.AddError("Read failed", err.Error())
		return false, diags
	}
	defer func() { _ = f.Close() }()
	if info, err = f.Stat(); err != nil {
		diags.AddError("Stat failed", err.Error())
		return false, diags
	}
	if err := checkRegularFile(target, info); err != nil {
		diags.AddAttributeError(path.Root("path"), "Not a regular file", capitalize(err.Error())+".")
		return true, diags
	}
	var sums checksums
	if m.Source.IsNull() && wo == nil {
		content, err := io.ReadAll(f)
		if err != nil {
			diags.AddError("Read failed", err.Error())
			return false, diags
		}
		if sums, err = checksumReader(bytes.NewReader(content)); err != nil {
			diags.AddError("Read failed", err.Error())
			return false, diags
		}
		refreshContent(m, content)
	} else if sums, err = checksumReader(f); err != nil {
		diags.AddError("Read failed", err.Error())
		return false, diags
	}
	if wo != nil {
		wo.Drift = !wo.matches(sums.sha256)
		m.ContentSHA256, m.ContentMD5 = types.StringNull(), types.StringNull()
	} else {
		m.ContentSHA256 = types.StringValue(sums.sha256)
		m.ContentMD5 = types.StringValue(sums.md5)
	}

	m.Mode = types.StringValue(reconcileMode(knownString(m.Mode), info.Mode()))
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", target))
		return true, diags
	}
	m.Owner = types.StringValue(reconcileOwner(knownString(m.Owner), st.Uid))
	m.Group = types.StringValue(reconcileGroup(knownString(m.Group), st.Gid))
	m.ID = m.Path
	return true, diags
}
