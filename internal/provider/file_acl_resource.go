package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*fileACLResource)(nil)
	_ resource.ResourceWithConfigure      = (*fileACLResource)(nil)
	_ resource.ResourceWithImportState    = (*fileACLResource)(nil)
	_ resource.ResourceWithValidateConfig = (*fileACLResource)(nil)
)

const defaultFileACLExclusive = true

// ACL entry types in the configuration.
const (
	aclTypeUser  = "user"
	aclTypeGroup = "group"
	aclTypeMask  = "mask"
	aclTypeOther = "other"
)

func NewFileACLResource() resource.Resource { return &fileACLResource{} }

type fileACLResource struct{ rootedResource }

type fileACLModel struct {
	Path           types.String `tfsdk:"path"`
	Entries        types.Set    `tfsdk:"entries"`
	DefaultEntries types.Set    `tfsdk:"default_entries"`
	Exclusive      types.Bool   `tfsdk:"exclusive"`
	ID             types.String `tfsdk:"id"`
}

type fileACLEntryModel struct {
	Type        types.String `tfsdk:"type"`
	Name        types.String `tfsdk:"name"`
	Permissions types.String `tfsdk:"permissions"`
}

var fileACLEntryType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"type":        types.StringType,
	"name":        types.StringType,
	"permissions": types.StringType,
}}

func (r *fileACLResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_file_acl"
}

func fileACLEntrySchema() schema.NestedAttributeObject {
	return schema.NestedAttributeObject{
		Attributes: map[string]schema.Attribute{
			"type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "`\"user\"`, `\"group\"`, `\"mask\"` or `\"other\"`. " +
					"A `user` entry without `name` is the file's owner and a `group` entry without `name` its owning group; these and `other` are the permission bits of the mode.",
				Validators: []validator.String{stringvalidator.OneOf(aclTypeUser, aclTypeGroup, aclTypeMask, aclTypeOther)},
			},
			"name": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "User or group name, or numeric UID or GID, for a named `user` or `group` entry. Must not be set for `mask` and `other`. " +
					"Names are looked up in the host's user database when the ACL is applied, like `owner` of `sysutils_file`.",
				Validators: []validator.String{stringCheck("ACL entry name", validateACLEntryName)},
			},
			"permissions": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Permissions in the fixed-width form `getfacl` prints: three characters, `r` or `-`, `w` or `-`, and `x` or `-`, such as `\"rwx\"`, `\"r-x\"` or `\"---\"`.",
				Validators: []validator.String{stringCheck("ACL permissions", func(s string) error {
					_, err := parseACLPerm(s)
					return err
				})},
			},
		},
	}
}

func (r *fileACLResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the POSIX access ACL of an existing file or directory and, for a directory, its default ACL, like `setfacl`. " +
			"The ACLs are read and written directly as the `system.posix_acl_access` and `system.posix_acl_default` extended attributes, so no `setfacl` binary is needed. " +
			"On destroy, the extended entries the resource managed are removed and the owner, group and other permissions are left as they are.",
		Attributes: map[string]schema.Attribute{
			"path": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute path of an existing regular file or directory. " +
					"Must be in canonical form (no `.`/`..` segments, duplicate or trailing slashes) and must not be `/`. " +
					"A symlink at the path is refused, never followed. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{absolutePath()},
			},
			"entries": schema.SetNestedAttribute{
				Required: true,
				MarkdownDescription: "Entries of the access ACL. Named `user` and `group` entries grant access to further users and groups. " +
					"Unless a `mask` entry is given, the mask is computed as the union of the named entries and the owning group, as `setfacl` does. " +
					"Owner, owning group and `other` entries that are not listed keep their current permissions. " +
					"May be empty, which with `exclusive = true` removes every named entry.",
				NestedObject: fileACLEntrySchema(),
			},
			"default_entries": schema.SetNestedAttribute{
				Optional: true,
				MarkdownDescription: "Entries of the default ACL, which files and directories created inside the directory inherit. Only valid if `path` is a directory. " +
					"If the directory has no default ACL yet, owner, owning group and `other` entries that are not listed are copied from the access ACL, as `setfacl -d` does. " +
					"If unset, the default ACL is not managed.",
				NestedObject: fileACLEntrySchema(),
			},
			"exclusive": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(defaultFileACLExclusive),
				MarkdownDescription: "Whether `entries` (and `default_entries`) are the complete list of named entries. " +
					"With `true`, named entries that are not listed are removed, and show up as drift. " +
					"With `false`, only the listed entries are ensured, and entries added by others are left alone. Defaults to `true`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `path`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// validateACLEntryName reports why s can't name the user or group of an ACL
// entry: it must be a user or group name or a numeric ID.
func validateACLEntryName(s string) error {
	if _, ok := parseID(s); ok {
		return nil
	}
	return validateAccountName(s)
}

// ValidateConfig checks what the attribute validators can't: that name is
// only set where it means something, and that no entry is listed twice with
// different permissions.
func (r *fileACLResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg fileACLModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, attrName := range []string{"entries", "default_entries"} {
		set := cfg.Entries
		if attrName == "default_entries" {
			set = cfg.DefaultEntries
		}
		if set.IsNull() || set.IsUnknown() {
			continue
		}
		var entries []fileACLEntryModel
		resp.Diagnostics.Append(set.ElementsAs(ctx, &entries, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		seen := map[string]bool{}
		for _, e := range entries {
			if e.Type.IsUnknown() || e.Name.IsUnknown() {
				continue
			}
			t := e.Type.ValueString()
			if !e.Name.IsNull() && (t == aclTypeMask || t == aclTypeOther) {
				resp.Diagnostics.AddAttributeError(path.Root(attrName), "Invalid ACL entry",
					fmt.Sprintf("A %q entry has no name, but name %q is set.", t, e.Name.ValueString()))
				continue
			}
			key := t + ":" + e.Name.ValueString()
			if seen[key] {
				resp.Diagnostics.AddAttributeError(path.Root(attrName), "Duplicate ACL entry",
					fmt.Sprintf("The entry %s is listed more than once, with different permissions.", describeACLEntry(e)))
			}
			seen[key] = true
		}
	}
}

// describeACLEntry returns e in setfacl's form without permissions, such as
// "user:alice" or "mask".
func describeACLEntry(e fileACLEntryModel) string {
	if e.Name.IsNull() {
		return e.Type.ValueString()
	}
	return e.Type.ValueString() + ":" + e.Name.ValueString()
}

// resolvedACLEntry is a configured entry together with the ACL entry it
// resolves to.
type resolvedACLEntry struct {
	model fileACLEntryModel
	entry aclEntry
}

// resolveACLEntries resolves configured entries to ACL entries, looking up
// user and group names. Two entries that resolve to the same ACL entry, such
// as "alice" and alice's UID, are an error.
func resolveACLEntries(models []fileACLEntryModel) ([]resolvedACLEntry, error) {
	out := make([]resolvedACLEntry, 0, len(models))
	seen := map[aclKey]fileACLEntryModel{}
	for _, m := range models {
		e, err := resolveACLEntry(m)
		if err != nil {
			return nil, err
		}
		if prev, ok := seen[e.key()]; ok {
			return nil, fmt.Errorf("entries %s and %s both refer to the ACL entry %s", describeACLEntry(prev), describeACLEntry(m), e.key())
		}
		seen[e.key()] = m
		out = append(out, resolvedACLEntry{model: m, entry: e})
	}
	return out, nil
}

func resolveACLEntry(m fileACLEntryModel) (aclEntry, error) {
	perm, err := parseACLPerm(m.Permissions.ValueString())
	if err != nil {
		return aclEntry{}, err
	}
	named := !m.Name.IsNull()
	name := m.Name.ValueString()
	e := aclEntry{Perm: perm}
	switch t := m.Type.ValueString(); {
	case t == aclTypeUser && !named:
		e.Tag = aclUserObj
	case t == aclTypeGroup && !named:
		e.Tag = aclGroupObj
	case t == aclTypeUser:
		uid, err := lookupUID(name)
		if err != nil {
			return aclEntry{}, fmt.Errorf("looking up user %q: %w", name, err)
		}
		e.Tag, e.ID = aclUser, uint32(uid)
	case t == aclTypeGroup:
		gid, err := lookupGID(name)
		if err != nil {
			return aclEntry{}, fmt.Errorf("looking up group %q: %w", name, err)
		}
		e.Tag, e.ID = aclGroup, uint32(gid)
	case named:
		return aclEntry{}, fmt.Errorf("a %q entry has no name, but name %q is set", t, name)
	case t == aclTypeMask:
		e.Tag = aclMask
	case t == aclTypeOther:
		e.Tag = aclOther
	default:
		return aclEntry{}, fmt.Errorf("unknown ACL entry type %q", t)
	}
	return e, nil
}

func aclEntries(r []resolvedACLEntry) []aclEntry {
	out := make([]aclEntry, len(r))
	for i := range r {
		out[i] = r[i].entry
	}
	return out
}

// aclEntryModel returns the configuration form of e. Named entries get the
// user or group name, or the numeric ID if it has none.
func aclEntryModel(e aclEntry) fileACLEntryModel {
	m := fileACLEntryModel{Name: types.StringNull(), Permissions: types.StringValue(formatACLPerm(e.Perm))}
	switch e.Tag {
	case aclUserObj:
		m.Type = types.StringValue(aclTypeUser)
	case aclUser:
		m.Type, m.Name = types.StringValue(aclTypeUser), types.StringValue(uidName(e.ID))
	case aclGroupObj:
		m.Type = types.StringValue(aclTypeGroup)
	case aclGroup:
		m.Type, m.Name = types.StringValue(aclTypeGroup), types.StringValue(gidName(e.ID))
	case aclMask:
		m.Type = types.StringValue(aclTypeMask)
	default:
		m.Type = types.StringValue(aclTypeOther)
	}
	return m
}

// observedACLEntries returns the entries to record in state for the actual
// ACL, given the managed entries: each managed entry that the ACL has, with
// its actual permissions and its configured spelling, and every entry that
// applying the managed entries would remove or change although they don't
// list it (see aclDrift). With no drift, that is exactly the managed entries.
// Managed entries whose names no longer resolve are left out, so that the
// plan adds them back.
func observedACLEntries(actual posixACL, managed []fileACLEntryModel, exclusive bool) []fileACLEntryModel {
	var resolved []resolvedACLEntry
	for _, m := range managed {
		if e, err := resolveACLEntry(m); err == nil {
			resolved = append(resolved, resolvedACLEntry{model: m, entry: e})
		}
	}
	matched, unexpected := aclDrift(actual, aclEntries(resolved), exclusive)
	out := []fileACLEntryModel{}
	seen := map[string]bool{}
	add := func(m fileACLEntryModel) {
		k := m.Type.ValueString() + "\x00" + m.Name.ValueString() + "\x00" + m.Permissions.ValueString()
		if !seen[k] {
			seen[k] = true
			out = append(out, m)
		}
	}
	for _, r := range resolved {
		if e, ok := matched[r.entry.key()]; ok {
			m := r.model
			m.Permissions = types.StringValue(formatACLPerm(e.Perm))
			add(m)
		}
	}
	for _, e := range unexpected {
		add(aclEntryModel(e))
	}
	sort.SliceStable(out, func(i, j int) bool {
		return describeACLEntry(out[i]) < describeACLEntry(out[j])
	})
	return out
}

// fileACLSpec is the part of a fileACLModel that says what to apply.
type fileACLSpec struct {
	entries []fileACLEntryModel
	// defaults is nil if the default ACL is not managed.
	defaults  []fileACLEntryModel
	exclusive bool
}

func (m *fileACLModel) spec(ctx context.Context) (fileACLSpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	s := fileACLSpec{exclusive: m.Exclusive.IsNull() || m.Exclusive.ValueBool(), entries: []fileACLEntryModel{}}
	if !m.Entries.IsNull() && !m.Entries.IsUnknown() {
		diags.Append(m.Entries.ElementsAs(ctx, &s.entries, false)...)
	}
	if !m.DefaultEntries.IsNull() && !m.DefaultEntries.IsUnknown() {
		s.defaults = []fileACLEntryModel{}
		diags.Append(m.DefaultEntries.ElementsAs(ctx, &s.defaults, false)...)
	}
	return s, diags
}

func (r *fileACLResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan fileACLModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, diags := plan.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(plan.Path.ValueString(), nil, spec)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *fileACLResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state fileACLModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, diags := plan.spec(ctx)
	resp.Diagnostics.Append(diags...)
	prev, diags := state.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(plan.Path.ValueString(), &prev, spec)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// resolveACLPath checks the managed path p, which comes from state on read
// and delete, and resolves it inside root_dir.
func (r *fileACLResource) resolveACLPath(p string) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	// Config validation already enforces this; state is not validated.
	if err := validateAbsolutePath(p); err != nil {
		diags.AddAttributeError(path.Root("path"), "Invalid path", capitalize(err.Error())+".")
		return "", diags
	}
	return resolvePathAttr(r.root(), p, false)
}

// aclPathDiag returns an error diagnostic on the path attribute for err.
func aclPathDiag(err error) diag.Diagnostic {
	return diag.NewAttributeErrorDiagnostic(path.Root("path"), "Accessing ACL", capitalize(err.Error())+".")
}

// apply sets the ACLs of p to spec. prev is the previously applied spec on
// update, whose managed named entries are removed if spec no longer lists
// them, or nil on create.
func (r *fileACLResource) apply(p string, prev *fileACLSpec, spec fileACLSpec) diag.Diagnostics {
	var diags diag.Diagnostics
	target, d := r.resolveACLPath(p)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	unlock, err := lockFileForEdit(target)
	if err != nil {
		diags.AddAttributeError(path.Root("path"), "Locking file", capitalize(err.Error())+".")
		return diags
	}
	defer unlock()
	f, err := openACLTarget(target)
	if err != nil {
		diags.Append(aclPathDiag(err))
		return diags
	}
	defer func() { _ = f.Close() }()

	if spec.defaults != nil && !f.isDir {
		diags.AddAttributeError(path.Root("default_entries"), "Default ACL on a non-directory",
			fmt.Sprintf("Path %q is not a directory; only directories have a default ACL. Remove default_entries.", p))
		return diags
	}
	want, err := resolveACLEntries(spec.entries)
	if err != nil {
		diags.AddAttributeError(path.Root("entries"), "Invalid ACL entry", capitalize(err.Error())+".")
		return diags
	}
	var wantDefault []resolvedACLEntry
	if spec.defaults != nil {
		if wantDefault, err = resolveACLEntries(spec.defaults); err != nil {
			diags.AddAttributeError(path.Root("default_entries"), "Invalid ACL entry", capitalize(err.Error())+".")
			return diags
		}
	}
	// Entries managed before but no longer listed are removed. Names that
	// no longer resolve can't be removed; with exclusive, they are anyway.
	var dropped, droppedDefault []aclEntry
	if prev != nil {
		dropped = removedACLEntries(resolvableACLEntries(prev.entries), aclEntries(want))
		droppedDefault = removedACLEntries(resolvableACLEntries(prev.defaults), aclEntries(wantDefault))
	}

	access, err := f.readAccessACL()
	if err != nil {
		diags.Append(aclPathDiag(err))
		return diags
	}
	next := applyACLEntries(stripListed(access, dropped), aclEntries(want), spec.exclusive)
	if !next.equal(access) {
		if err := f.writeAccessACL(next); err != nil {
			diags.Append(aclWriteDiag("entries", err))
			return diags
		}
	}

	if !f.isDir {
		return diags
	}
	cur, err := f.readDefaultACL()
	if err != nil {
		diags.Append(aclPathDiag(err))
		return diags
	}
	var nextDefault posixACL
	switch {
	case spec.defaults != nil:
		start := cur
		if start == nil {
			// setfacl -d copies the missing base entries from the access
			// ACL.
			start = next.baseEntries()
		}
		nextDefault = applyACLEntries(stripListed(start, droppedDefault), aclEntries(wantDefault), spec.exclusive)
	case prev != nil && prev.defaults != nil && cur != nil:
		// default_entries was removed from the configuration: remove what
		// it managed, as destroy would.
		nextDefault = stripACLEntries(cur, resolvableACLEntries(prev.defaults), prev.exclusive)
		if nextDefault.isMinimal() {
			nextDefault = nil
		}
	default:
		return diags
	}
	if cur != nil && nextDefault != nil && nextDefault.equal(cur) {
		return diags
	}
	if err := f.writeDefaultACL(nextDefault); err != nil {
		diags.Append(aclWriteDiag("default_entries", err))
	}
	return diags
}

// stripListed returns a without the named entries in drop.
func stripListed(a posixACL, drop []aclEntry) posixACL {
	if len(drop) == 0 {
		return a
	}
	keys := map[aclKey]bool{}
	for _, e := range drop {
		keys[e.key()] = true
	}
	return a.without(func(e aclEntry) bool { return keys[e.key()] })
}

// resolvableACLEntries resolves the entries that still resolve, skipping the
// others: entries of users or groups deleted since, which can't be found in
// the ACL by name any more.
func resolvableACLEntries(models []fileACLEntryModel) []aclEntry {
	var out []aclEntry
	for _, m := range models {
		if e, err := resolveACLEntry(m); err == nil {
			out = append(out, e)
		}
	}
	return out
}

func aclWriteDiag(attrName string, err error) diag.Diagnostic {
	detail := capitalize(err.Error()) + "."
	if isACLNotSupported(err) {
		detail += " Mount the file system with ACL support (for ext4, the acl option, which is the default), or use a file system that has it."
	}
	return diag.NewAttributeErrorDiagnostic(path.Root(attrName), "Setting ACL", detail)
}

func (r *fileACLResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state fileACLModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// entries is required, so it is only null right after import, which
	// adopts every named entry of both ACLs.
	imported := state.Entries.IsNull()
	spec, diags := state.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, diags := r.resolveACLPath(state.Path.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	f, err := openACLTarget(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		resp.State.RemoveResource(ctx)
		return
	case err != nil:
		resp.Diagnostics.Append(aclPathDiag(err))
		return
	}
	defer func() { _ = f.Close() }()

	access, err := f.readAccessACL()
	if err != nil {
		resp.Diagnostics.Append(aclPathDiag(err))
		return
	}
	entries, diags := types.SetValueFrom(ctx, fileACLEntryType, observedACLEntries(access, spec.entries, spec.exclusive))
	resp.Diagnostics.Append(diags...)
	state.Entries = entries

	if spec.defaults != nil || imported {
		cur, err := f.readDefaultACL()
		if err != nil {
			resp.Diagnostics.Append(aclPathDiag(err))
			return
		}
		// A missing default ACL, or a file where a directory was, is
		// recorded as null, so that the plan adds default_entries back.
		state.DefaultEntries = types.SetNull(fileACLEntryType)
		if cur != nil {
			defaults, diags := types.SetValueFrom(ctx, fileACLEntryType, observedACLEntries(cur, spec.defaults, spec.exclusive))
			resp.Diagnostics.Append(diags...)
			state.DefaultEntries = defaults
		}
	}
	if state.Exclusive.IsNull() {
		state.Exclusive = types.BoolValue(defaultFileACLExclusive)
	}
	state.ID = state.Path
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *fileACLResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileACLModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spec, diags := state.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, diags := r.resolveACLPath(state.Path.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	unlock, err := lockFileForEdit(target)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("path"), "Locking file", capitalize(err.Error())+".")
		return
	}
	defer unlock()
	f, err := openACLTarget(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case errors.Is(err, errNotACLTarget):
		// A symlink or special file now occupies the path; its ACL is not
		// ours to change.
		resp.Diagnostics.AddWarning("File already replaced",
			fmt.Sprintf("%s. Its ACL was left untouched.", capitalize(err.Error())))
		return
	case err != nil:
		resp.Diagnostics.Append(aclPathDiag(err))
		return
	}
	defer func() { _ = f.Close() }()

	access, err := f.readAccessACL()
	if err != nil {
		resp.Diagnostics.Append(aclPathDiag(err))
		return
	}
	if next := stripACLEntries(access, resolvableACLEntries(spec.entries), spec.exclusive); !next.equal(access) {
		if err := f.writeAccessACL(next); err != nil {
			resp.Diagnostics.Append(aclWriteDiag("entries", err))
			return
		}
	}

	if spec.defaults == nil || !f.isDir {
		return
	}
	cur, err := f.readDefaultACL()
	if err != nil {
		resp.Diagnostics.Append(aclPathDiag(err))
		return
	}
	if cur == nil {
		return
	}
	// A default ACL without extended entries would still override the
	// umask of new files, so it is removed as a whole.
	next := stripACLEntries(cur, resolvableACLEntries(spec.defaults), spec.exclusive)
	if next.isMinimal() {
		next = nil
	} else if next.equal(cur) {
		return
	}
	if err := f.writeDefaultACL(next); err != nil {
		resp.Diagnostics.Append(aclWriteDiag("default_entries", err))
	}
}

func (r *fileACLResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAbsolutePath(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the absolute path of the file or directory: %s.", strings.TrimSuffix(err.Error(), ".")))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("exclusive"), defaultFileACLExclusive)...)
}
