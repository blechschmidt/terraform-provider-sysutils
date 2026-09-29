package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                     = (*packageRepositoryResource)(nil)
	_ resource.ResourceWithConfigure        = (*packageRepositoryResource)(nil)
	_ resource.ResourceWithImportState      = (*packageRepositoryResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*packageRepositoryResource)(nil)
	_ resource.ResourceWithConfigValidators = (*packageRepositoryResource)(nil)
)

// Keys of the resource's private state.
const (
	// repoPrivateKeySHA256 records the SHA-256 of the signing key fetched
	// from signing_key_url when it was written, so that a key file changed
	// on disk since then plans a new fetch.
	repoPrivateKeySHA256 = "key_sha256"
	// repoPrivateRefreshPending records that refresh_cache failed after the
	// repository was written, so that the next apply retries it.
	repoPrivateRefreshPending = "refresh_pending"
)

// repoConfig is the provider-level configuration of
// sysutils_package_repository. The zero value, or a nil pointer, writes
// files owned by root and fetches keys with a default HTTP client; tests set
// their own user and a client that trusts their TLS test server.
type repoConfig struct {
	uid, gid   uint32
	httpClient *http.Client
	// root, if set and the provider has no root_dir, is where the managed
	// paths are, instead of the host root. Unlike root_dir it does not
	// disable refresh_cache, so that tests can cover it without touching
	// the host's repositories.
	root *fsRoot
}

func (c *repoConfig) owner() (uid, gid uint32) {
	if c == nil {
		return 0, 0
	}
	return c.uid, c.gid
}

func (c *repoConfig) client() *http.Client {
	if c == nil || c.httpClient == nil {
		return &http.Client{Timeout: signingKeyFetchTimeout}
	}
	return c.httpClient
}

func NewPackageRepositoryResource() resource.Resource { return &packageRepositoryResource{} }

type packageRepositoryResource struct {
	rootedResource
	cfg *repoConfig
	pkg *packageConfig
}

type packageRepositoryModel struct {
	Name             types.String `tfsdk:"name"`
	Manager          types.String `tfsdk:"manager"`
	Description      types.String `tfsdk:"description"`
	URIs             types.List   `tfsdk:"uris"`
	Types            types.List   `tfsdk:"types"`
	Suites           types.List   `tfsdk:"suites"`
	Components       types.List   `tfsdk:"components"`
	Architectures    types.List   `tfsdk:"architectures"`
	Tag              types.String `tfsdk:"tag"`
	Enabled          types.Bool   `tfsdk:"enabled"`
	GPGCheck         types.Bool   `tfsdk:"gpg_check"`
	SigningKey       types.String `tfsdk:"signing_key"`
	SigningKeyURL    types.String `tfsdk:"signing_key_url"`
	RefreshCache     types.Bool   `tfsdk:"refresh_cache"`
	Path             types.String `tfsdk:"path"`
	SigningKeyPath   types.String `tfsdk:"signing_key_path"`
	SigningKeySHA256 types.String `tfsdk:"signing_key_sha256"`
	Content          types.String `tfsdk:"content"`
	ID               types.String `tfsdk:"id"`
}

func (r *packageRepositoryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_package_repository"
}

func (r *packageRepositoryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stringList := func(what string, check func(string) error) []validator.List {
		return []validator.List{
			listvalidator.SizeAtLeast(1),
			listvalidator.UniqueValues(),
			listvalidator.ValueStringsAre(stringCheck(what, check)),
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an OS package repository, the companion of `sysutils_package`: " +
			"for apt, a deb822 file `/etc/apt/sources.list.d/<name>.sources`; for dnf and yum, a file `/etc/yum.repos.d/<name>.repo`; for apk, a line in `/etc/apk/repositories`, after a comment line naming the repository. " +
			"A signing key, given inline or fetched from a URL, is stored ASCII-armored in `/etc/apt/keyrings/<name>.asc` (apt) or `/etc/pki/rpm-gpg/RPM-GPG-KEY-<name>` (dnf and yum). " +
			"Files are written atomically with mode `0644`, owned by `root`, below the provider's `root_dir` if that is set, and removed on destroy. " +
			"Refresh reads them back, so changes made outside Terraform show up as a planned change of `content` or `signing_key_sha256`. " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the repository, such as `\"docker\"`: the file name without extension for apt, dnf and yum, and the dnf repository id. " +
					"Letters, digits, `_`, `.` and `-`, starting with a letter or digit, at most 100 bytes. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("repository name", validateRepoName)},
			},
			"manager": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(packageManagerAuto),
				MarkdownDescription: "Package manager whose repository to manage: `\"apt\"`, `\"dnf\"`, `\"yum\"` or `\"apk\"`. " +
					"`\"auto\"` uses apt if `/etc/apt` is a directory, else dnf or yum if one of `/etc/yum.repos.d`, `/etc/dnf` and `/etc/yum` is, else apk if `/etc/apk` is, looking below `root_dir` if that is set. " +
					"Defaults to `\"auto\"`. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf(packageManagerAuto, packageManagerApt, packageManagerDnf, packageManagerYum, packageManagerApk)},
			},
			"description": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Human-readable name of the repository: dnf's `name` (which defaults to `name`), `X-Repolib-Name` for apt, and part of the comment line for apk. " +
					"A single line of at most 256 bytes.",
				Validators: []validator.String{stringCheck("description", validateRepoDescription)},
			},
			"uris": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				MarkdownDescription: "Base URLs of the repository: apt's `URIs`, dnf's `baseurl`, the apk repository. `http`, `https` and `file` URLs only, without white space, `#` or credentials; apk takes exactly one. " +
					"dnf variables such as `$releasever` and `$basearch` are passed through.",
				Validators: stringList("repository URI", validateRepoURI),
			},
			"types": schema.ListAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "apt only: `\"deb\"` and/or `\"deb-src\"`. Defaults to `[\"deb\"]`.",
				Validators:          stringList("apt type", validateAptType),
			},
			"suites": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "apt only, and required for apt: suites or codenames, such as `[\"bookworm\"]`. " +
					"A suite ending with `/` is the path of a flat repository, such as `[\"./\"]`, and takes no `components`.",
				Validators: stringList("apt suite", validateAptSuite),
			},
			"components": schema.ListAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "apt only: components, such as `[\"main\", \"contrib\"]`. Required unless the suite is the path of a flat repository.",
				Validators:          stringList("apt component", validateAptComponent),
			},
			"architectures": schema.ListAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "apt only: architectures to download indexes for, such as `[\"amd64\"]`. If unset, apt uses all configured architectures.",
				Validators:          stringList("apt architecture", validateAptArchitecture),
			},
			"tag": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "apk only: tag of the repository, written as `@tag` before the URL. Packages from a tagged repository are only installed when asked for as `name@tag`. " +
					"1 to 64 letters, digits, `_` or `-`.",
				Validators: []validator.String{stringCheck("apk tag", validateApkTag)},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether the package manager uses the repository: `Enabled: no` for apt, `enabled=0` for dnf and yum, a commented-out line for apk. " +
					"Defaults to `true`.",
			},
			"gpg_check": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "dnf and yum only: whether package signatures are checked (`gpgcheck`). Defaults to `true`; `false` is refused for apt and apk, which have no such per-repository switch.",
			},
			"signing_key": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "ASCII-armored OpenPGP public key that signs the repository (apt) or its packages (dnf, yum), stored in `signing_key_path` and referred to by `Signed-By` or `gpgkey`. " +
					"Conflicts with `signing_key_url`. Not supported for apk, whose keys must be named after their signer.",
			},
			"signing_key_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "`https` or `file` URL of the OpenPGP public key. " +
					"For apt, the provider fetches the key during apply, converts a binary (`.gpg`) key to ASCII armor and stores it in `signing_key_path`; a `file` URL is read from the host, as `root_dir` does not apply to it. " +
					"The key is fetched again when the URL changes or the stored key no longer matches what was fetched, not when the key behind the URL changes. " +
					"For dnf and yum, the URL is written to `gpgkey` as is, and dnf fetches it itself. " +
					"Plain `http` is refused, since anyone on the network path could replace the key. Conflicts with `signing_key`.",
				Validators: []validator.String{stringCheck("signing key URL", validateSigningKeyURL)},
			},
			"refresh_cache": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Refresh the package index (`apt-get update`, `dnf makecache`, `yum makecache` or `apk update`) after the repository was created, changed or removed, so that `sysutils_package` resources that depend on this one find its packages. " +
					"A failed refresh fails the apply and is retried by the next one. Not supported with `root_dir`, since it would refresh the host's index. Defaults to `false`.",
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the repository file: `/etc/apt/sources.list.d/<name>.sources`, `/etc/yum.repos.d/<name>.repo` or `/etc/apk/repositories`.",
			},
			"signing_key_path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the stored signing key: `/etc/apt/keyrings/<name>.asc` or `/etc/pki/rpm-gpg/RPM-GPG-KEY-<name>`; null if no key is stored.",
			},
			"signing_key_sha256": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "SHA-256 of the stored signing key, as written (ASCII-armored). A key file changed, removed or given another mode or owner outside Terraform shows up as a change of this attribute.",
			},
			"content": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Contents of the repository file, or, for apk, the managed lines of `/etc/apk/repositories`. " +
					"Refresh reads them back, so any change made outside Terraform shows up as a planned change of this attribute; a file with another mode or owner reads as null.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *packageRepositoryResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.Conflicting(path.MatchRoot("signing_key"), path.MatchRoot("signing_key_url")),
	}
}

func (r *packageRepositoryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.repo
		r.pkg = data.pkg
		r.fsRoot = data.root
	}
}

// paths returns the root that managed paths are relative to.
func (r *packageRepositoryResource) paths() *fsRoot {
	if r.cfg != nil && r.cfg.root != nil && r.root().isHost() {
		return r.cfg.root
	}
	return r.root()
}

// family returns the repository family for manager kind, detecting it
// below root_dir for "auto".
func (r *packageRepositoryResource) family(kind string) (string, error) {
	if f := repoFamily(kind); f != "" {
		return f, nil
	}
	root := r.paths()
	return detectRepoFamily(func(p string) bool {
		host, err := root.resolveFollow(p)
		if err != nil {
			return false
		}
		info, err := os.Stat(host)
		return err == nil && info.IsDir()
	})
}

// listValues returns the elements of l, and whether l and all of them are
// known. A null list has no elements.
func listValues(ctx context.Context, l types.List) ([]string, bool, diag.Diagnostics) {
	if l.IsUnknown() {
		return nil, false, nil
	}
	if l.IsNull() {
		return nil, true, nil
	}
	var elems []types.String
	diags := l.ElementsAs(ctx, &elems, false)
	if diags.HasError() {
		return nil, false, diags
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if e.IsUnknown() {
			return nil, false, diags
		}
		out = append(out, e.ValueString())
	}
	return out, true, diags
}

// listOrNull returns values as a list, or a null list if there are none.
func listOrNull(values []string) types.List {
	if len(values) == 0 {
		return types.ListNull(types.StringType)
	}
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	l, _ := types.ListValue(types.StringType, elems)
	return l
}

// repoSpecFromModel returns the repository m describes, without its
// signing key. known is false if any part of it is unknown.
func repoSpecFromModel(ctx context.Context, m *packageRepositoryModel) (spec repoSpec, known bool, diags diag.Diagnostics) {
	for _, v := range []types.String{m.Name, m.Description, m.Tag, m.SigningKey, m.SigningKeyURL} {
		if v.IsUnknown() {
			return spec, false, diags
		}
	}
	if m.Enabled.IsUnknown() || m.GPGCheck.IsUnknown() {
		return spec, false, diags
	}
	spec = repoSpec{
		name:        m.Name.ValueString(),
		description: m.Description.ValueString(),
		tag:         m.Tag.ValueString(),
		enabled:     m.Enabled.IsNull() || m.Enabled.ValueBool(),
		gpgCheck:    m.GPGCheck.IsNull() || m.GPGCheck.ValueBool(),
	}
	known = true
	for _, f := range []struct {
		l   types.List
		dst *[]string
	}{
		{m.URIs, &spec.uris}, {m.Types, &spec.types}, {m.Suites, &spec.suites},
		{m.Components, &spec.components}, {m.Architectures, &spec.architectures},
	} {
		values, ok, d := listValues(ctx, f.l)
		diags.Append(d...)
		known = known && ok
		*f.dst = values
	}
	return spec, known && !diags.HasError(), diags
}

// keyForValidate returns what repoSpec.validate checks as the key of m:
// signing_key_url, or a placeholder for an inline key, which apk refuses.
func keyForValidate(m *packageRepositoryModel) string {
	if !m.SigningKey.IsNull() {
		return "inline"
	}
	return m.SigningKeyURL.ValueString()
}

// storesKey reports whether the key m configures is stored in a file for
// family: inline keys always are, URL keys only for apt.
func (m *packageRepositoryModel) storesKey(family string) bool {
	return !m.SigningKey.IsNull() || (!m.SigningKeyURL.IsNull() && family == repoFamilyApt)
}

// ModifyPlan checks the repository against the rules of its manager and
// sets the computed attributes to what apply writes, so that anything else
// read back by refresh plans an update.
func (r *packageRepositoryResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan packageRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.RefreshCache.ValueBool() && !r.root().isHost() {
		resp.Diagnostics.AddAttributeError(path.Root("refresh_cache"), "Not supported with root_dir",
			fmt.Sprintf("refresh_cache would refresh the package index of the running host, not of the tree below root_dir = %q. Set refresh_cache = false.", r.root().String()))
		return
	}
	var prior *packageRepositoryModel
	if !req.State.Raw.IsNull() {
		prior = &packageRepositoryModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, prior)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	unknown := types.StringUnknown()
	plan.Path, plan.SigningKeyPath, plan.SigningKeySHA256, plan.Content = unknown, unknown, unknown, unknown
	if plan.Name.IsUnknown() {
		plan.ID = unknown
	} else {
		plan.ID = plan.Name
	}
	family := ""
	if !plan.Manager.IsUnknown() && !plan.Name.IsUnknown() {
		// A failed detection is only an error at apply time: the tree below
		// root_dir may be created by the same apply.
		family, _ = r.family(plan.Manager.ValueString())
	}
	if family != "" {
		name := plan.Name.ValueString()
		plan.Path = types.StringValue(repoPath(family, name))
		spec, known, diags := repoSpecFromModel(ctx, &plan)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		if known {
			spec.signingKeyURL = keyForValidate(&plan)
			if err := spec.validate(family); err != nil {
				resp.Diagnostics.AddError("Invalid repository", fmt.Sprintf("Invalid %s repository: %s.", familyDisplay(family), err))
				return
			}
			spec.signingKeyURL = ""
		}
		keyKnown := !plan.SigningKey.IsUnknown() && !plan.SigningKeyURL.IsUnknown()
		if keyKnown {
			plan.SigningKeyPath, plan.SigningKeySHA256 = types.StringNull(), types.StringNull()
			if plan.storesKey(family) {
				keyPath := signingKeyPath(family, name)
				plan.SigningKeyPath = types.StringValue(keyPath)
				spec.signingKeyPath = keyPath
				switch {
				case !plan.SigningKey.IsNull():
					key, err := normalizeSigningKey([]byte(plan.SigningKey.ValueString()))
					if err != nil {
						resp.Diagnostics.AddAttributeError(path.Root("signing_key"), "Invalid signing key", capitalize(err.Error())+".")
						return
					}
					plan.SigningKeySHA256 = types.StringValue(sha256Hex(key))
				case prior != nil && prior.SigningKeyURL.Equal(plan.SigningKeyURL) && prior.SigningKeyPath.Equal(plan.SigningKeyPath) &&
					!prior.SigningKeySHA256.IsNull() && privateString(ctx, req.Private, repoPrivateKeySHA256) == prior.SigningKeySHA256.ValueString():
					// The stored key is still the one fetched from this URL.
					plan.SigningKeySHA256 = prior.SigningKeySHA256
				default:
					plan.SigningKeySHA256 = unknown
				}
			} else if !plan.SigningKeyURL.IsNull() {
				spec.signingKeyURL = plan.SigningKeyURL.ValueString()
			}
		}
		if known && keyKnown {
			plan.Content = types.StringValue(string(spec.render(family)))
		}
	}
	if prior != nil && plan.RefreshCache.ValueBool() && privateBool(ctx, req.Private, repoPrivateRefreshPending) {
		// Forces an update, which retries the refresh.
		plan.Content = unknown
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// privateGetter is implemented by the framework's private state wrappers.
type privateGetter interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateSetter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

func privateString(ctx context.Context, p privateGetter, key string) string {
	if p == nil {
		return ""
	}
	raw, diags := p.GetKey(ctx, key)
	var s string
	if diags.HasError() || len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func privateBool(ctx context.Context, p privateGetter, key string) bool {
	if p == nil {
		return false
	}
	raw, diags := p.GetKey(ctx, key)
	var b bool
	if diags.HasError() || len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return false
	}
	return b
}

// setPrivate sets key to the JSON encoding of v, or removes it if v is the
// zero value.
func setPrivate(ctx context.Context, p privateSetter, key string, v any) diag.Diagnostics {
	if p == nil {
		return nil
	}
	switch v {
	case "", false, nil:
		return p.SetKey(ctx, key, nil)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("Encoding private state", err.Error())
		return diags
	}
	return p.SetKey(ctx, key, raw)
}

func (r *packageRepositoryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan packageRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Changing the repositories while a package manager command runs, or
	// two index refreshes at once, would fail or install from a
	// half-configured repository set.
	unlock, err := lockPackageManager(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Locking package manager", capitalize(err.Error())+".")
		return
	}
	defer unlock()
	res := r.apply(ctx, &plan, nil, false)
	resp.Diagnostics.Append(res.diags...)
	// A repository written before a failure is recorded, so that it is
	// tainted and replaced or destroyed by the next apply.
	if res.diags.HasError() && !res.written {
		return
	}
	resp.Diagnostics.Append(setPrivate(ctx, resp.Private, repoPrivateKeySHA256, res.fetchedKeySHA256)...)
	resp.Diagnostics.Append(setPrivate(ctx, resp.Private, repoPrivateRefreshPending, res.refreshPending)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *packageRepositoryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior packageRepositoryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Changing the repositories while a package manager command runs, or
	// two index refreshes at once, would fail or install from a
	// half-configured repository set.
	unlock, err := lockPackageManager(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Locking package manager", capitalize(err.Error())+".")
		return
	}
	defer unlock()
	pending := privateBool(ctx, req.Private, repoPrivateRefreshPending)
	res := r.apply(ctx, &plan, &prior, pending)
	resp.Diagnostics.Append(res.diags...)
	if res.diags.HasError() && !res.written {
		return
	}
	fetched := res.fetchedKeySHA256
	if fetched == "" && res.keptFetchedKey {
		fetched = privateString(ctx, req.Private, repoPrivateKeySHA256)
	}
	resp.Diagnostics.Append(setPrivate(ctx, resp.Private, repoPrivateKeySHA256, fetched)...)
	resp.Diagnostics.Append(setPrivate(ctx, resp.Private, repoPrivateRefreshPending, res.refreshPending)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// applyResult is the outcome of apply.
type applyResult struct {
	diags diag.Diagnostics
	// written is set once anything on disk was changed.
	written bool
	// fetchedKeySHA256 is the SHA-256 of a key fetched from
	// signing_key_url and written in this apply.
	fetchedKeySHA256 string
	// keptFetchedKey is set if a previously fetched key was left alone.
	keptFetchedKey bool
	// refreshPending is set if refresh_cache failed.
	refreshPending bool
}

// apply writes the repository and its key as plan describes and sets the
// computed attributes of plan. prior is the state before an update, or nil
// on create. retryRefresh forces a refresh of the package index even if
// nothing changed, after an earlier one failed.
func (r *packageRepositoryResource) apply(ctx context.Context, plan, prior *packageRepositoryModel, retryRefresh bool) (res applyResult) {
	diags := &res.diags
	create := prior == nil
	name := plan.Name.ValueString()
	if err := validateRepoName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return res
	}
	kind := plan.Manager.ValueString()
	family, err := r.family(kind)
	if err != nil {
		diags.AddAttributeError(path.Root("manager"), "Package manager not detected", capitalize(err.Error())+".")
		return res
	}
	if plan.RefreshCache.ValueBool() && !r.root().isHost() {
		diags.AddAttributeError(path.Root("refresh_cache"), "Not supported with root_dir", "refresh_cache cannot be used with root_dir.")
		return res
	}
	spec, known, d := repoSpecFromModel(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return res
	}
	if !known {
		diags.AddError("Unknown values", "All attributes of the repository must be known at apply time.")
		return res
	}
	spec.signingKeyURL = keyForValidate(plan)
	if err := spec.validate(family); err != nil {
		diags.AddError("Invalid repository", fmt.Sprintf("Invalid %s repository: %s.", familyDisplay(family), err))
		return res
	}
	spec.signingKeyURL = ""
	uid, gid := r.cfg.owner()
	resolve := func(p string) (string, bool) {
		host, err := r.paths().resolve(p)
		if err != nil {
			diags.AddError("Unable to resolve path", fmt.Sprintf("Resolving %s: %s.", p, capitalize(err.Error())))
			return "", false
		}
		return host, true
	}

	// The key comes first, so that the repository never refers to a key
	// that is not there yet.
	plannedSHA := plan.SigningKeySHA256
	keyPath := ""
	// createdKey is the host path of the key file if this apply created it.
	createdKey := ""
	plan.SigningKeyPath, plan.SigningKeySHA256 = types.StringNull(), types.StringNull()
	if plan.storesKey(family) {
		keyPath = signingKeyPath(family, name)
		spec.signingKeyPath = keyPath
		keyHost, ok := resolve(keyPath)
		if !ok {
			return res
		}
		ownKey := prior != nil && prior.SigningKeyPath.ValueString() == keyPath
		var key []byte
		switch {
		case !plan.SigningKey.IsNull():
			if key, err = normalizeSigningKey([]byte(plan.SigningKey.ValueString())); err != nil {
				diags.AddAttributeError(path.Root("signing_key"), "Invalid signing key", capitalize(err.Error())+".")
				return res
			}
		case ownKey && !plannedSHA.IsUnknown() && !plannedSHA.IsNull() && storedKeySHA256(keyHost, uid, gid) == plannedSHA.ValueString():
			// The plan found the key fetched from this URL still in place.
			res.keptFetchedKey = true
			plan.SigningKeySHA256 = plannedSHA
		default:
			raw, err := fetchSigningKey(ctx, r.cfg.client(), plan.SigningKeyURL.ValueString())
			if err == nil {
				key, err = normalizeSigningKey(raw)
			}
			if err != nil {
				diags.AddAttributeError(path.Root("signing_key_url"), "Fetching signing key",
					fmt.Sprintf("Fetching %s: %s.", plan.SigningKeyURL.ValueString(), capitalize(err.Error())))
				return res
			}
			res.fetchedKeySHA256 = sha256Hex(key)
		}
		if key != nil {
			// A key file of another repository or of the administrator is
			// never taken over.
			changed, err := writeRepoFile(keyHost, key, uid, gid, !ownKey)
			if err != nil {
				diags.Append(repoWriteError(keyPath, "signing_key_path", err)...)
				return res
			}
			res.written = res.written || changed
			if !ownKey {
				createdKey = keyHost
			}
			plan.SigningKeySHA256 = types.StringValue(sha256Hex(key))
		}
		plan.SigningKeyPath = types.StringValue(keyPath)
	} else if !plan.SigningKeyURL.IsNull() {
		spec.signingKeyURL = plan.SigningKeyURL.ValueString()
	}

	p := repoPath(family, name)
	host, ok := resolve(p)
	if !ok {
		return res
	}
	content := spec.render(family)
	var changed bool
	if family == repoFamilyApk {
		owner := newApkOwner(&spec)
		if prior != nil {
			owner = newApkOwner(&spec, apkSpecOf(ctx, prior))
		}
		changed, err = writeApkBlock(host, name, content, create, owner)
	} else {
		changed, err = writeRepoFile(host, content, uid, gid, create)
	}
	if err != nil {
		diags.Append(repoWriteError(p, "name", err)...)
		if createdKey != "" {
			// Without the repository, the new key is removed again, and
			// nothing is recorded: a create that failed because the file
			// exists must not taint a resource whose destroy would remove
			// that file.
			if err := removeCronFile(createdKey); err != nil {
				diags.AddError("Removing signing key", fmt.Sprintf("Removing %s after the repository could not be written: %s.", keyPath, err))
			}
			res.written = false
		}
		return res
	}
	res.written = res.written || changed

	// A key that is no longer configured is removed after the repository
	// stopped referring to it.
	if prior != nil && !prior.SigningKeyPath.IsNull() && prior.SigningKeyPath.ValueString() != keyPath {
		if old := signingKeyPath(family, name); prior.SigningKeyPath.ValueString() == old {
			if oldHost, ok := resolve(old); ok {
				if err := removeCronFile(oldHost); err != nil {
					diags.AddError("Removing signing key", fmt.Sprintf("Removing %s: %s.", old, err))
				} else {
					res.written = true
				}
			}
		}
	}

	plan.ID = types.StringValue(name)
	plan.Path = types.StringValue(p)
	plan.Content = types.StringValue(string(content))
	if diags.HasError() {
		return res
	}
	if res.written {
		r.invalidateCache(family)
	}
	if plan.RefreshCache.ValueBool() && (res.written || retryRefresh) {
		if err := r.refreshCache(ctx, family, kind); err != nil {
			diags.AddAttributeError(path.Root("refresh_cache"), "Refreshing package index",
				fmt.Sprintf("The repository was written, but refreshing the package index failed; the next apply retries it. %s.", capitalize(err.Error())))
			res.refreshPending = true
			res.written = true
		}
	}
	return res
}

// repoWriteError converts an error from writing path p.
func repoWriteError(p, attr string, err error) diag.Diagnostics {
	var diags diag.Diagnostics
	if errors.Is(err, errCronFileExists) || errors.Is(err, errRepoExists) {
		diags.AddAttributeError(path.Root(attr), "Repository already exists",
			fmt.Sprintf("%s already exists. Import the repository with \"terraform import\" instead, or remove the file.", p))
		return diags
	}
	diags.AddError("Writing repository", fmt.Sprintf("Writing %s: %s.", p, err))
	return diags
}

func (r *packageRepositoryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state packageRepositoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	gone, diags := r.refresh(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if gone {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// stateFamily returns the family of a resource in state: that of its
// manager, or, for "auto", the one whose path the state records, so that a
// later change of the detected manager does not lose track of the files.
func (r *packageRepositoryResource) stateFamily(m *packageRepositoryModel) (string, error) {
	if f := repoFamily(m.Manager.ValueString()); f != "" {
		return f, nil
	}
	for _, f := range []string{repoFamilyApt, repoFamilyRpm, repoFamilyApk} {
		if !m.Path.IsNull() && m.Path.ValueString() == repoPath(f, m.Name.ValueString()) {
			return f, nil
		}
	}
	return r.family(m.Manager.ValueString())
}

// refresh updates m from the files on disk. It reports gone if the
// repository does not exist. An imported resource, recognized by its unset
// uris, gets all its attributes from the files.
func (r *packageRepositoryResource) refresh(ctx context.Context, m *packageRepositoryModel) (gone bool, diags diag.Diagnostics) {
	name := m.Name.ValueString()
	// State is not validated by the schema; never read an arbitrary file.
	if err := validateRepoName(name); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	imported := m.URIs.IsNull()
	if m.Manager.IsNull() {
		m.Manager = types.StringValue(packageManagerAuto)
	}
	family, err := r.stateFamily(m)
	if err != nil {
		diags.AddAttributeError(path.Root("manager"), "Package manager not detected", capitalize(err.Error())+".")
		return false, diags
	}
	uid, gid := r.cfg.owner()
	p := repoPath(family, name)
	host, err := r.paths().resolve(p)
	if err != nil {
		diags.AddError("Unable to resolve path", fmt.Sprintf("Resolving %s: %s.", p, capitalize(err.Error())))
		return false, diags
	}
	data, snap, err := readCronFile(host)
	if err != nil {
		diags.AddError("Reading repository", fmt.Sprintf("Reading %s: %s.", p, err))
		return false, diags
	}

	var parsed parsedRepo
	found := snap != nil
	content := types.StringNull()
	switch family {
	case repoFamilyApt:
		if found {
			parsed = parseAptSources(name, data)
			if parsed.extraStanzas > 0 {
				diags.AddWarning("Several repositories in one file",
					fmt.Sprintf("%s contains %d stanzas. sysutils_package_repository manages a single one; the next apply removes the others.", p, parsed.extraStanzas+1))
			}
		}
	case repoFamilyRpm:
		if found {
			var ok bool
			if parsed, ok = parseYumRepo(name, data); !ok && imported {
				diags.AddError("Cannot import repository", fmt.Sprintf("%s has no section [%s].", p, name))
				return false, diags
			}
		}
	case repoFamilyApk:
		if found {
			lines := parseTextFile(data).lines
			// An imported repository takes whatever line follows its
			// marker.
			var owner *apkOwner
			if !imported {
				owner = newApkOwner(apkSpecOf(ctx, m))
			}
			start, end, count := apkBlock(lines, name, owner)
			found = start >= 0
			if found {
				parsed, _ = parseApkRepo(name, lines, owner)
				content = types.StringValue(strings.Join(lines[start:end], "\n") + "\n")
				if count > 1 {
					diags.AddWarning("Repository listed several times",
						fmt.Sprintf("%s contains %d entries for %s. The next apply removes all but the first.", p, count, name))
					content = types.StringValue(content.ValueString() + "# (and further entries)\n")
				}
			}
		}
	}
	if !found {
		if imported {
			diags.AddError("Cannot import repository", fmt.Sprintf("Repository %s not found in %s.", name, p))
		}
		return true, diags
	}
	if family != repoFamilyApk && snap.mode == repoFileMode && snap.uid == uid && snap.gid == gid {
		content = types.StringValue(strings.ToValidUTF8(string(data), "�"))
	}

	keyPath := signingKeyPath(family, name)
	storesKey := !m.SigningKeyPath.IsNull() && m.SigningKeyPath.ValueString() == keyPath
	if imported {
		storesKey = keyPath != "" && parsed.signedBy == keyPath
	}
	m.SigningKeyPath, m.SigningKeySHA256 = types.StringNull(), types.StringNull()
	var keyData []byte
	if storesKey {
		m.SigningKeyPath = types.StringValue(keyPath)
		keyHost, err := r.paths().resolve(keyPath)
		if err != nil {
			diags.AddError("Unable to resolve path", fmt.Sprintf("Resolving %s: %s.", keyPath, capitalize(err.Error())))
			return false, diags
		}
		data, snap, err := readCronFile(keyHost)
		if err != nil {
			diags.AddError("Reading signing key", fmt.Sprintf("Reading %s: %s.", keyPath, err))
			return false, diags
		}
		if snap != nil && snap.mode == repoFileMode && snap.uid == uid && snap.gid == gid {
			keyData = data
			m.SigningKeySHA256 = types.StringValue(sha256Hex(data))
		}
	}

	if imported {
		m.Description = stringOrNull(parsed.description)
		m.URIs = listOrNull(parsed.uris)
		if len(m.URIs.Elements()) == 0 {
			// uris must not stay null, or the next refresh would import
			// again; an empty list plans the configured URIs.
			m.URIs = types.ListValueMust(types.StringType, nil)
		}
		if len(parsed.types) == 1 && parsed.types[0] == "deb" {
			parsed.types = nil
		}
		m.Types = listOrNull(parsed.types)
		m.Suites = listOrNull(parsed.suites)
		m.Components = listOrNull(parsed.components)
		m.Architectures = listOrNull(parsed.architectures)
		m.Tag = stringOrNull(parsed.tag)
		m.Enabled = types.BoolValue(parsed.enabled)
		m.GPGCheck = types.BoolValue(parsed.gpgCheck || family != repoFamilyRpm)
		m.SigningKey, m.SigningKeyURL = types.StringNull(), stringOrNull(parsed.signingKeyURL)
		if storesKey && keyData != nil {
			m.SigningKey = types.StringValue(strings.ToValidUTF8(string(keyData), "�"))
		}
		m.RefreshCache = types.BoolValue(false)
	}
	m.ID = types.StringValue(name)
	m.Path = types.StringValue(p)
	m.Content = content
	return false, diags
}

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func (r *packageRepositoryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state packageRepositoryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never remove an arbitrary file.
	if err := validateRepoName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	family, err := r.stateFamily(&state)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("manager"), "Package manager not detected", capitalize(err.Error())+".")
		return
	}
	p := repoPath(family, name)
	host, err := r.paths().resolve(p)
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve path", fmt.Sprintf("Resolving %s: %s.", p, capitalize(err.Error())))
		return
	}
	// Changing the repositories while a package manager command runs, or
	// two index refreshes at once, would fail or install from a
	// half-configured repository set.
	unlock, err := lockPackageManager(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Locking package manager", capitalize(err.Error())+".")
		return
	}
	defer unlock()
	if family == repoFamilyApk {
		err = removeApkBlock(host, name, newApkOwner(apkSpecOf(ctx, &state)))
	} else {
		err = removeCronFile(host)
	}
	if err != nil {
		resp.Diagnostics.AddError("Removing repository", fmt.Sprintf("Removing %s: %s.", p, err))
		return
	}
	if keyPath := signingKeyPath(family, name); keyPath != "" && state.SigningKeyPath.ValueString() == keyPath {
		keyHost, err := r.paths().resolve(keyPath)
		if err == nil {
			err = removeCronFile(keyHost)
		}
		if err != nil {
			resp.Diagnostics.AddError("Removing signing key", fmt.Sprintf("Removing %s: %s.", keyPath, err))
			return
		}
	}
	r.invalidateCache(family)
	if state.RefreshCache.ValueBool() && r.root().isHost() {
		if err := r.refreshCache(ctx, family, state.Manager.ValueString()); err != nil {
			resp.Diagnostics.AddWarning("Refreshing package index",
				fmt.Sprintf("The repository was removed, but refreshing the package index failed: %s.", capitalize(err.Error())))
		}
	}
}

// ImportState imports a repository by name, or by "<manager>:<name>" to
// select a manager other than the detected one.
func (r *packageRepositoryResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	manager, name := packageManagerAuto, req.ID
	if m, n, ok := strings.Cut(req.ID, ":"); ok {
		manager, name = m, n
		if repoFamily(manager) == "" {
			resp.Diagnostics.AddError("Invalid import ID",
				fmt.Sprintf("Import ID must be <name> or <manager>:<name>, where manager is apt, dnf, yum or apk, not %q.", manager))
			return
		}
	}
	if err := validateRepoName(name); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be <name> or <manager>:<name>: %s.", capitalize(err.Error())))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("manager"), manager)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
}

// packageKinds lists the sysutils_package manager kinds of family.
func packageKinds(family string) []string {
	switch family {
	case repoFamilyApt:
		return []string{packageManagerApt}
	case repoFamilyRpm:
		return []string{packageManagerDnf, packageManagerYum}
	case repoFamilyApk:
		return []string{packageManagerApk}
	}
	return nil
}

// invalidateCache makes sysutils_package resources with update_cache
// refresh the package index of family again, now that its repositories
// changed. The caller holds the package-manager lock.
func (r *packageRepositoryResource) invalidateCache(family string) {
	cfg := r.pkg.orDefault()
	for _, k := range packageKinds(family) {
		delete(cfg.cacheUpdated, k)
	}
}

// refreshCache refreshes the package index of family with the configured
// manager kind, or with the first of the family's managers that is
// installed.
func (r *packageRepositoryResource) refreshCache(ctx context.Context, family, kind string) error {
	cfg := r.pkg.orDefault()
	kinds := packageKinds(family)
	if repoFamily(kind) == family {
		kinds = []string{kind}
	}
	var mgr packageManager
	var errs []error
	for _, k := range kinds {
		m, err := cfg.resolve(k)
		if err == nil {
			mgr = m
			break
		}
		errs = append(errs, err)
	}
	if mgr == nil {
		return errors.Join(errs...)
	}
	if err := mgr.UpdateCache(ctx); err != nil {
		return err
	}
	if cfg.cacheUpdated == nil {
		cfg.cacheUpdated = map[string]bool{}
	}
	cfg.cacheUpdated[mgr.Kind()] = true
	return nil
}

// errRepoExists is returned by writeApkBlock when a new repository is
// already listed.
var errRepoExists = errors.New("repository already exists")

// writeRepoFile makes the file at p contain data, with mode 0644 and owned
// by uid:gid, atomically, and reports whether anything changed. With
// create set, an existing file is an error. It shares writeCronFile, whose
// files have the same mode.
func writeRepoFile(p string, data []byte, uid, gid uint32, create bool) (bool, error) {
	cur, snap, err := readCronFile(p)
	if err != nil {
		return false, err
	}
	if snap != nil && !create && bytes.Equal(cur, data) && snap.mode == repoFileMode && snap.uid == uid && snap.gid == gid {
		return false, nil
	}
	return true, writeCronFile(p, data, uid, gid, create)
}

// storedKeySHA256 returns the SHA-256 of the key file at p if it is a
// regular file with the mode and owner the resource gives it, or "".
func storedKeySHA256(p string, uid, gid uint32) string {
	data, snap, err := readCronFile(p)
	if err != nil || snap == nil || snap.mode != repoFileMode || snap.uid != uid || snap.gid != gid {
		return ""
	}
	return sha256Hex(data)
}

// editApkRepositories applies edit to /etc/apk/repositories at p, keeping
// the file's mode, ownership and extended attributes, and writes the result
// atomically if edit reports a change. A missing file is created with mode
// 0644.
func editApkRepositories(p string, edit func(t *textFile) (bool, error)) (bool, error) {
	unlock, err := lockFileForEdit(p)
	if err != nil {
		return false, err
	}
	defer unlock()
	data, snap, err := readRegularFileNoFollow(p, maxRepoFileSize)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	t := parseTextFile(data)
	changed, err := edit(t)
	if err != nil || !changed {
		return false, err
	}
	return true, writeManagedFile(p, t.bytes(), snap, repoFileMode)
}

// writeApkBlock makes the managed lines of repository name in the apk
// repositories file p equal block. With create set, an existing entry is
// an error.
func writeApkBlock(p, name string, block []byte, create bool, owner *apkOwner) (bool, error) {
	lines := parseTextFile(block).lines
	return editApkRepositories(p, func(t *textFile) (bool, error) {
		if start, _, _ := apkBlock(t.lines, name, owner); create && start >= 0 {
			return false, fmt.Errorf("%s: %w", p, errRepoExists)
		}
		return setApkBlock(t, name, lines, owner), nil
	})
}

// removeApkBlock removes the managed lines of repository name, as far as
// owner owns them, from p. The file itself is kept, even if empty.
func removeApkBlock(p, name string, owner *apkOwner) error {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	_, err := editApkRepositories(p, func(t *textFile) (bool, error) {
		return removeApkBlocks(t, name, 0, owner), nil
	})
	return err
}

// apkSpecOf returns the URIs and tag of the apk repository m, which is all
// an apkOwner needs. Unknown or invalid values are left out.
func apkSpecOf(ctx context.Context, m *packageRepositoryModel) *repoSpec {
	uris, _, _ := listValues(ctx, m.URIs)
	return &repoSpec{uris: uris, tag: m.Tag.ValueString()}
}
