package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*remoteFileResource)(nil)
	_ resource.ResourceWithImportState    = (*remoteFileResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*remoteFileResource)(nil)
	_ resource.ResourceWithValidateConfig = (*remoteFileResource)(nil)
	_ resource.ResourceWithConfigure      = (*remoteFileResource)(nil)
)

const (
	defaultRemoteFileTimeout = "60s"
	defaultRemoteFileMaxSize = 1 << 30

	// remoteFilePrivateSHA256 is the private state key recording the SHA-256
	// of the last download, so that an unverified file changed on disk since
	// then plans a new download.
	remoteFilePrivateSHA256 = "downloaded_sha256"
)

func NewRemoteFileResource() resource.Resource { return &remoteFileResource{} }

type remoteFileResource struct{ rootedResource }

type remoteFileModel struct {
	URL             types.String `tfsdk:"url"`
	Path            types.String `tfsdk:"path"`
	Checksum        types.String `tfsdk:"checksum"`
	AllowUnverified types.Bool   `tfsdk:"allow_unverified"`
	Headers         types.Map    `tfsdk:"headers"`
	Timeout         types.String `tfsdk:"timeout"`
	MaxSizeBytes    types.Int64  `tfsdk:"max_size_bytes"`
	ForceRedownload types.Bool   `tfsdk:"force_redownload"`
	Mode            types.String `tfsdk:"mode"`
	Owner           types.String `tfsdk:"owner"`
	Group           types.String `tfsdk:"group"`
	SHA256          types.String `tfsdk:"sha256"`
	ID              types.String `tfsdk:"id"`
}

func (r *remoteFileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_remote_file"
}

func (r *remoteFileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Downloads an `http` or `https` URL to a local file and verifies its checksum, like Ansible's `get_url` module.",
		Attributes: map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The `http` or `https` URL to download. Must not contain user information (`user:password@`); pass credentials in `headers`. " +
					"The URL is fetched only when the file is created or must be downloaded again, never during refresh. " +
					"Changing it downloads again only without `checksum`: with a checksum, the content it pins does not depend on where it comes from.",
				Validators: []validator.String{downloadURL()},
			},
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of the local file. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"Missing parent directories are created with mode `0755`. A symlink or directory at `path` is refused. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"checksum": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Expected checksum of the file, `sha256:<hex>` or `sha512:<hex>`. " +
					"A download that does not match is discarded before it reaches `path`. " +
					"Refresh hashes the local file; if it no longer matches, the plan shows its actual checksum being changed back to this value and apply downloads the file again. " +
					"Required unless `allow_unverified` is `true`.",
				Validators: []validator.String{checksumString()},
			},
			"allow_unverified": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Allow a download without `checksum`. Its content is then whatever the server returns. " +
					"The file is downloaded again when `url` changes, when the local file no longer has the SHA-256 recorded at the last download, or with `force_redownload`. " +
					"Defaults to `false`.",
			},
			"headers": schema.MapAttribute{
				Optional:    true,
				Sensitive:   true,
				ElementType: types.StringType,
				MarkdownDescription: "HTTP request headers, such as `Authorization`. The values are sensitive. " +
					"They are sent to the host of `url` only, and removed from requests that follow a redirect to another host or port.",
				Validators: []validator.Map{httpHeaders()},
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultRemoteFileTimeout),
				MarkdownDescription: "Maximum time for one download, including redirects and reading the response, as a Go duration such as `\"60s\"` or `\"10m\"`. " +
					"Defaults to `\"60s\"`.",
				Validators: []validator.String{positiveDuration()},
			},
			"max_size_bytes": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(defaultRemoteFileMaxSize),
				MarkdownDescription: "Largest response accepted, in bytes. A larger response, whatever its `Content-Length`, fails the download and leaves nothing behind. " +
					"Defaults to 1 GiB (`1073741824`).",
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"force_redownload": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Download the file again on every apply, even if the local file matches. " +
					"Every plan then shows an update. With `checksum`, the new download must still match it. Defaults to `false`.",
			},
			"mode": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultFileMode),
				MarkdownDescription: "Octal mode with 3 or 4 digits, optionally with a leading zero, such as `\"0644\"` or `\"0755\"`. " +
					"Applied with an explicit `chmod` before the file is renamed into place. Defaults to `\"0644\"`.",
				Validators: []validator.String{octalMode()},
			},
			"owner": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Username or numeric UID that should own the file. Requires privileges to change. " +
					"If unset, a new file belongs to the user running Terraform, and a replaced file keeps its owner.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"group": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name or numeric GID of the file. Requires privileges to change. " +
					"If unset, a new file gets the group of the user running Terraform, and a replaced file keeps its group.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"sha256": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Hex-encoded SHA-256 of the local file. Known at plan time when `checksum` is a SHA-256 checksum, " +
					"otherwise when no download is planned.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *remoteFileResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config remoteFileModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.AllowUnverified.IsUnknown() || config.Checksum.IsUnknown() {
		return
	}
	unverified := config.AllowUnverified.ValueBool()
	if config.Checksum.IsNull() && !unverified {
		resp.Diagnostics.AddAttributeError(path.Root("checksum"), "Missing checksum",
			"Set checksum to the expected \"sha256:<hex>\" or \"sha512:<hex>\" of the file, "+
				"or set allow_unverified = true to accept whatever the server returns.")
		return
	}
	if config.Checksum.IsNull() && !config.URL.IsUnknown() && strings.HasPrefix(strings.ToLower(config.URL.ValueString()), "http:") {
		resp.Diagnostics.AddAttributeWarning(path.Root("url"), "Unverified download over plain http",
			"Without checksum, anyone on the network path to the server can replace the file's content. Use https or set checksum.")
	}
}

// ModifyPlan decides whether apply downloads the file, and plans sha256
// accordingly: unknown for a download, unless a SHA-256 checksum already
// says what it will be, and the recorded value otherwise. Refresh never
// fetches the URL; it records the local file's actual checksum in
// checksum, so drift shows up here as a changed checksum.
func (r *remoteFileResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan remoteFileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state *remoteFileModel
	if !req.State.Raw.IsNull() {
		state = &remoteFileModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, state)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	downloaded, diags := getPrivateString(ctx, req.Private, remoteFilePrivateSHA256)
	resp.Diagnostics.Append(diags...)

	sha := types.StringUnknown()
	if !remoteFileDownloadPlanned(&plan, state, downloaded) {
		sha = state.SHA256
	} else if want, err := parseChecksum(plan.Checksum.ValueString()); err == nil && want.algo == checksumSHA256 &&
		!plan.Checksum.IsUnknown() && !plan.ForceRedownload.ValueBool() {
		// force_redownload needs an unknown value: an unchanged plan would
		// not call Update at all.
		sha = types.StringValue(want.hex)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("sha256"), sha)...)
}

// remoteFileDownloadPlanned reports whether applying plan over state may
// download the file. downloaded is the SHA-256 recorded at the last
// download, or "" if unknown (after import). Update makes the final
// decision against the file on disk.
func remoteFileDownloadPlanned(plan, state *remoteFileModel, downloaded string) bool {
	switch {
	case state == nil, state.SHA256.IsNull(), state.SHA256.IsUnknown():
		return true
	case plan.ForceRedownload.IsUnknown() || plan.ForceRedownload.ValueBool():
		return true
	case plan.Checksum.IsUnknown() || plan.URL.IsUnknown():
		return true
	case !plan.Checksum.IsNull():
		// Refresh has replaced the stored checksum with the actual one if
		// the file changed. Only the spelling of a matching checksum may
		// differ.
		return !strings.EqualFold(plan.Checksum.ValueString(), state.Checksum.ValueString())
	default:
		return plan.URL.ValueString() != state.URL.ValueString() ||
			(downloaded != "" && downloaded != state.SHA256.ValueString())
	}
}

func (r *remoteFileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan remoteFileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Config validation already enforces this; re-check as defense in depth.
	if err := validateAbsolutePath(plan.Path.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return
	}
	downloaded, diags := r.apply(ctx, &plan, nil, "")
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setPrivateJSON(ctx, resp.Private, remoteFilePrivateSHA256, downloaded)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *remoteFileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state remoteFileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, diags := resolvePathAttr(r.root(), state.Path.ValueString(), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := refreshRemoteFile(target, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *remoteFileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state remoteFileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior, diags := getPrivateString(ctx, req.Private, remoteFilePrivateSHA256)
	resp.Diagnostics.Append(diags...)
	downloaded, diags := r.apply(ctx, &plan, &state, prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(setPrivateJSON(ctx, resp.Private, remoteFilePrivateSHA256, downloaded)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply makes the file at plan's path match plan: it downloads the URL if
// the local file does not match (see needsDownload), and otherwise only
// corrects its mode and ownership. state is nil on create; prior is the
// SHA-256 recorded at the last download. On success plan holds the refreshed
// values, and apply returns the SHA-256 to record as the last download.
func (r *remoteFileResource) apply(ctx context.Context, plan, state *remoteFileModel, prior string) (downloaded string, diags diag.Diagnostics) {
	target, d := resolvePathAttr(r.root(), plan.Path.ValueString(), false)
	diags.Append(d...)
	if diags.HasError() {
		return "", diags
	}
	mode, err := parseMode(plan.Mode.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("mode"), "Invalid mode", err.Error())
		return "", diags
	}
	dreq, d := downloadRequestFrom(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return "", diags
	}

	local, err := readLocalFile(target, dreq.checksum != nil && dreq.checksum.algo == checksumSHA512)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		local = nil
	case err != nil:
		diags.AddAttributeError(path.Root("path"), "Unable to read file", capitalize(err.Error())+".")
		return "", diags
	}

	// Owner and group are unknown when not configured; ValueString returns
	// "" for unknown values, meaning "leave unchanged". On update, only
	// what changed is applied, so that an unrelated change does not need
	// the privileges to (re)assign ownership.
	owner, group := plan.Owner.ValueString(), plan.Group.ValueString()
	if state != nil {
		if plan.Owner.Equal(state.Owner) {
			owner = ""
		}
		if plan.Group.Equal(state.Group) {
			group = ""
		}
	}

	downloaded = prior
	if needsDownload(plan, state, local, prior) {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			diags.AddError("Creating parent directory", err.Error())
			return "", diags
		}
		file, err := downloadToTemp(ctx, dreq, target)
		if err != nil {
			diags.AddAttributeError(path.Root("url"), "Download failed", capitalize(err.Error())+".")
			return "", diags
		}
		if err := explainImmutable(file.install(target, mode, owner, group), target, filepath.Dir(target)); err != nil {
			diags.AddError("Installing downloaded file", capitalize(err.Error())+".")
			return "", diags
		}
		downloaded = file.digests.sha256
	} else if err := setRemoteFileAttrs(target, mode, owner, group); err != nil {
		diags.AddError("Setting file attributes", capitalize(err.Error())+".")
		return "", diags
	}
	if downloaded == "" && local != nil {
		// Adopted an existing file that matches, or imported: treat its
		// content as the last download.
		downloaded = local.digests.sha256
	}

	found, d := refreshRemoteFile(target, plan)
	diags.Append(d...)
	if !found && !diags.HasError() {
		diags.AddError("Installing downloaded file", fmt.Sprintf("File %q disappeared immediately after it was written.", target))
	}
	return downloaded, diags
}

// needsDownload reports whether the file must be downloaded: it is missing,
// force_redownload is set, it does not match the checksum, or, without
// checksum, the URL changed or the file changed since the last download.
func needsDownload(plan, state *remoteFileModel, local *localFileState, prior string) bool {
	switch {
	case local == nil, plan.ForceRedownload.ValueBool():
		return true
	case !plan.Checksum.IsNull():
		want, err := parseChecksum(plan.Checksum.ValueString())
		return err != nil || local.digests.get(want.algo) != want.hex
	case state == nil:
		// Adopting an existing file without a checksum: its content is
		// unknown, so fetch it.
		return true
	default:
		return plan.URL.ValueString() != state.URL.ValueString() ||
			(prior != "" && prior != local.digests.sha256)
	}
}

// setRemoteFileAttrs applies mode, and owner and group unless empty, to the
// existing regular file at target, through a descriptor opened without
// following symlinks, with the file's shared-file lock held.
func setRemoteFileAttrs(target string, mode fs.FileMode, owner, group string) error {
	unlock, err := lockFileForEdit(target)
	if err != nil {
		return err
	}
	defer unlock()
	f, err := openNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		return err
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

// downloadRequestFrom builds the download request for m.
func downloadRequestFrom(ctx context.Context, m *remoteFileModel) (downloadRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	req := downloadRequest{url: m.URL.ValueString(), maxSize: m.MaxSizeBytes.ValueInt64()}
	if req.maxSize <= 0 {
		req.maxSize = defaultRemoteFileMaxSize
	}
	timeout := m.Timeout.ValueString()
	if timeout == "" {
		timeout = defaultRemoteFileTimeout
	}
	if err := validateDuration(timeout); err != nil {
		diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", capitalize(err.Error())+".")
		return req, diags
	}
	req.timeout, _ = time.ParseDuration(timeout)
	if !m.Checksum.IsNull() {
		want, err := parseChecksum(m.Checksum.ValueString())
		if err != nil {
			diags.AddAttributeError(path.Root("checksum"), "Invalid checksum", capitalize(err.Error())+".")
			return req, diags
		}
		req.checksum = &want
	} else if !m.AllowUnverified.ValueBool() {
		diags.AddAttributeError(path.Root("checksum"), "Missing checksum", "Set checksum, or allow_unverified = true.")
		return req, diags
	}
	if !m.Headers.IsNull() {
		diags.Append(m.Headers.ElementsAs(ctx, &req.headers, false)...)
	}
	return req, diags
}

// refreshRemoteFile refreshes m's sha256, checksum, mode, owner, group and
// id from the file at target, and reports found=false if it does not exist.
// If the file no longer matches m's checksum, checksum is set to the file's
// actual checksum with the same algorithm, so that the plan shows the drift
// and apply downloads the file again. Configured values equivalent to the
// actual ones (such as "644" and "0644", or a UID and its name) are kept.
func refreshRemoteFile(target string, m *remoteFileModel) (found bool, diags diag.Diagnostics) {
	var want *expectedChecksum
	if !m.Checksum.IsNull() && !m.Checksum.IsUnknown() {
		if c, err := parseChecksum(m.Checksum.ValueString()); err == nil {
			want = &c
		}
	}
	local, err := readLocalFile(target, want != nil && want.algo == checksumSHA512)
	if errors.Is(err, fs.ErrNotExist) {
		return false, diags
	}
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Unable to read file", capitalize(err.Error())+".")
		return true, diags
	}
	if want != nil {
		if got := local.digests.get(want.algo); got != want.hex {
			m.Checksum = types.StringValue(want.algo + ":" + got)
		}
	}
	m.SHA256 = types.StringValue(local.digests.sha256)
	m.Mode = types.StringValue(reconcileMode(knownString(m.Mode), local.info.Mode()))
	st, ok := local.info.Sys().(*syscall.Stat_t)
	if !ok {
		diags.AddError("Stat failed", fmt.Sprintf("Unable to determine ownership of %q on this platform.", target))
		return true, diags
	}
	m.Owner = types.StringValue(reconcileOwner(knownString(m.Owner), st.Uid))
	m.Group = types.StringValue(reconcileGroup(knownString(m.Group), st.Gid))
	m.ID = m.Path
	return true, diags
}

func (r *remoteFileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state remoteFileModel
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
	unlock, err := lockFileForEdit(target)
	if err != nil {
		resp.Diagnostics.AddError("Removing file", err.Error())
		return
	}
	defer unlock()
	// unlink never follows symlinks and never removes directories.
	if err := syscall.Unlink(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		resp.Diagnostics.AddError("Removing file", explainImmutable(&fs.PathError{Op: "unlink", Path: target, Err: err}, filepath.Dir(target)).Error())
		return
	}
	syncDir(filepath.Dir(target))
}

// ImportState imports the file at the path given as ID. url and checksum
// are unknown until the configuration sets them; the first apply after the
// import then records them without downloading, if the file matches.
func (r *remoteFileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the file: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_unverified"), false)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeout"), defaultRemoteFileTimeout)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("max_size_bytes"), int64(defaultRemoteFileMaxSize))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("force_redownload"), false)...)
}

// downloadURL validates an http or https URL for sysutils_remote_file.
func downloadURL() validator.String { return downloadURLValidator{} }

type downloadURLValidator struct{}

func (downloadURLValidator) Description(context.Context) string {
	return "must be an http or https URL without user information"
}

func (v downloadURLValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (downloadURLValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := validateDownloadURL(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid URL", capitalize(err.Error())+".")
	}
}

// checksumString validates a "sha256:<hex>" or "sha512:<hex>" checksum.
func checksumString() validator.String { return checksumValidator{} }

type checksumValidator struct{}

func (checksumValidator) Description(context.Context) string {
	return "must be sha256:<64 hex digits> or sha512:<128 hex digits>"
}

func (v checksumValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (checksumValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := parseChecksum(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid checksum", capitalize(err.Error())+".")
	}
}

// httpHeaders validates a map of HTTP request header names and values.
func httpHeaders() validator.Map { return httpHeadersValidator{} }

type httpHeadersValidator struct{}

func (httpHeadersValidator) Description(context.Context) string {
	return "must map valid HTTP header names to values without control characters"
}

func (v httpHeadersValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (httpHeadersValidator) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	seen := map[string]string{}
	for name, v := range req.ConfigValue.Elements() {
		if err := validateHeaderName(name); err != nil {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid header", capitalize(err.Error())+".")
			continue
		}
		canonical := http.CanonicalHeaderKey(name)
		if other, ok := seen[canonical]; ok {
			resp.Diagnostics.AddAttributeError(req.Path, "Duplicate header",
				fmt.Sprintf("Headers %q and %q are the same header; HTTP header names are case-insensitive.", other, name))
			continue
		}
		seen[canonical] = name
		s, ok := v.(types.String)
		if !ok || s.IsUnknown() || s.IsNull() {
			continue
		}
		// The value is sensitive, so it is not quoted in the message.
		if strings.ContainsAny(s.ValueString(), "\r\n\x00") {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid header",
				fmt.Sprintf("The value of header %q contains a line break or NUL character.", name))
		}
	}
}

// validateHeaderName checks that name is an HTTP token (RFC 9110) and not a
// header that the HTTP client computes itself.
func validateHeaderName(name string) error {
	if name == "" {
		return errors.New("header names must not be empty")
	}
	for _, c := range name {
		if c > 0x7e || c <= ' ' || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", c) {
			return fmt.Errorf("header name %q contains the invalid character %q", name, c)
		}
	}
	switch http.CanonicalHeaderKey(name) {
	case "Host", "Content-Length", "Transfer-Encoding", "Connection", "Accept-Encoding":
		return fmt.Errorf("header %q is set by the HTTP client and can't be configured", name)
	}
	return nil
}
