package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"golang.org/x/crypto/ssh"
)

var (
	_ resource.Resource                = (*sshAuthorizedKeyResource)(nil)
	_ resource.ResourceWithConfigure   = (*sshAuthorizedKeyResource)(nil)
	_ resource.ResourceWithImportState = (*sshAuthorizedKeyResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*sshAuthorizedKeyResource)(nil)
)

func NewSSHAuthorizedKeyResource() resource.Resource { return &sshAuthorizedKeyResource{} }

type sshAuthorizedKeyResource struct {
	hostOnlyResource
	cfg *sshKeyConfig
}

type sshAuthorizedKeyModel struct {
	User        types.String `tfsdk:"user"`
	Key         types.String `tfsdk:"key"`
	Options     types.List   `tfsdk:"options"`
	Comment     types.String `tfsdk:"comment"`
	Fingerprint types.String `tfsdk:"fingerprint"`
	Line        types.String `tfsdk:"line"`
	Path        types.String `tfsdk:"path"`
	ID          types.String `tfsdk:"id"`
}

func (r *sshAuthorizedKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_ssh_authorized_key"
}

func (r *sshAuthorizedKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one public key in a user's `~/.ssh/authorized_keys`, with its options and comment, like Ansible's `authorized_key` module. " +
			"Other lines of the file are left alone. A missing `~/.ssh` is created with mode `0700` and the file is always written with mode `0600`, both owned by the user; " +
			"nothing inside the home directory is followed if it is a symlink. On destroy, only the key's line is removed.",
		Attributes: map[string]schema.Attribute{
			"user": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the user whose `~/.ssh/authorized_keys` to edit. The user and their home directory must exist when the resource is applied; " +
					"reference a [`sysutils_user`](./user.md) with `create_home = true` to create them first. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{accountName()},
			},
			"key": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The OpenSSH public key, `<type> <base64 key>` optionally followed by a comment, as in a `.pub` file (for example `file(\"~/.ssh/id_ed25519.pub\")`; surrounding white space is ignored). " +
					"Checked at plan time. Must not start with options; use `options`. " +
					"A line of the file holds this key if it has the same key type and key data, whatever its options and comment. " +
					"Changing it replaces the old key's line in place.",
				Validators: []validator.String{stringCheck("OpenSSH public key", validateSSHPublicKey)},
			},
			"options": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Options that restrict the key, written before it separated by commas, such as `[\"from=\\\"10.0.0.0/8\\\"\", \"no-pty\", \"command=\\\"/usr/local/bin/backup\\\"\"]`. " +
					"Each is a name, or a name, `=` and a double-quoted value in which a double quote is escaped as `\\\"`. See `AUTHORIZED_KEYS FILE FORMAT` in `sshd(8)`.",
				Validators: []validator.List{listvalidator.ValueStringsAre(stringCheck("authorized_keys option", validateSSHKeyOption))},
			},
			"comment": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Comment written after the key, such as `\"alice@laptop\"`. Defaults to the comment at the end of `key`, if any; set it to `\"\"` to write no comment. " +
					"Must be a single line without leading or trailing white space.",
				Validators: []validator.String{stringCheck("comment", validateSSHKeyComment)},
			},
			"fingerprint": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "SHA-256 fingerprint of the key, as printed by `ssh-keygen -l`, such as `\"SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU\"`.",
			},
			"line": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The key's line in the file, as the provider writes it: `options`, the key and the comment. " +
					"Refresh reads it back from the file, so any change to the line, and any further line with the same key, shows up as a planned change.",
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the file, `<home>/.ssh/authorized_keys`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier, `<user>:<fingerprint>`.",
			},
		},
	}
}

func (r *sshAuthorizedKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.sshKey
		r.fsRoot = data.root
	}
}

// sshKeySpec is the validated desired state of a resource.
type sshKeySpec struct {
	pub     ssh.PublicKey
	options []string
	// keyComment is the comment at the end of key; comment is the one to
	// write.
	keyComment, comment string
}

func (s *sshKeySpec) line() string { return renderAuthorizedKey(s.pub, s.options, s.comment) }

func (s *sshKeySpec) fingerprint() string { return sshKeyFingerprint(s.pub) }

// spec validates m, whose values must all be known.
func (m *sshAuthorizedKeyModel) spec(ctx context.Context) (*sshKeySpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	pub, keyComment, err := parseSSHPublicKey(m.Key.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("key"), "Invalid OpenSSH public key", capitalize(err.Error())+".")
		return nil, diags
	}
	s := &sshKeySpec{pub: pub, keyComment: keyComment, comment: keyComment}
	if !m.Comment.IsNull() {
		s.comment = m.Comment.ValueString()
		if err := validateSSHKeyComment(s.comment); err != nil {
			diags.AddAttributeError(path.Root("comment"), "Invalid comment", capitalize(err.Error())+".")
		}
	}
	if !m.Options.IsNull() {
		diags.Append(m.Options.ElementsAs(ctx, &s.options, false)...)
		for _, o := range s.options {
			if err := validateSSHKeyOption(o); err != nil {
				diags.AddAttributeError(path.Root("options"), "Invalid authorized_keys option", capitalize(err.Error())+".")
			}
		}
	}
	if diags.HasError() {
		return nil, diags
	}
	return s, diags
}

func (m *sshAuthorizedKeyModel) allKnown() bool {
	return !m.User.IsUnknown() && !m.Key.IsUnknown() && !m.Comment.IsUnknown() && !m.Options.IsUnknown()
}

// ModifyPlan refuses to plan with root_dir and sets fingerprint, line and
// id to what apply writes, so that a line changed outside Terraform plans
// an update. path is left to the framework: it depends on the user's home
// directory at apply time.
func (r *sshAuthorizedKeyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	r.hostOnlyResource.ModifyPlan(ctx, req, resp)
	if req.Plan.Raw.IsNull() || resp.Diagnostics.HasError() {
		return
	}
	var plan sshAuthorizedKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.allKnown() {
		for _, a := range []string{"fingerprint", "line", "id"} {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(a), types.StringUnknown())...)
		}
		return
	}
	spec, diags := plan.spec(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("fingerprint"), spec.fingerprint())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("line"), spec.line())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), sshKeyID(plan.User.ValueString(), spec.fingerprint()))...)
}

func sshKeyID(user, fingerprint string) string { return user + ":" + fingerprint }

func (r *sshAuthorizedKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sshAuthorizedKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, nil)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sshAuthorizedKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state sshAuthorizedKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// apply makes the key in plan present with its options and comment. prev
// is the prior state on update and nil on create; if it holds another key,
// that key's lines are removed and the new key takes the place of the first
// one. On success the computed attributes of plan are set.
func (r *sshAuthorizedKeyResource) apply(ctx context.Context, plan, prev *sshAuthorizedKeyModel) diag.Diagnostics {
	spec, diags := plan.spec(ctx)
	if diags.HasError() {
		return diags
	}
	match := matchSSHKey(spec.pub)
	// The previous key's lines, if it is another key.
	var oldMatch sshKeyMatcher
	if prev != nil {
		if prevPub, _, err := parseSSHPublicKey(prev.Key.ValueString()); err == nil && !match(prevPub) {
			oldMatch = matchSSHKey(prevPub)
		}
	}

	account, err := r.cfg.lookupAccount(plan.User.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("user"), "Looking up user", capitalize(err.Error())+".")
		return diags
	}
	dir, err := openSSHDir(account, true)
	if err != nil {
		diags.AddError("Opening ~/.ssh", capitalize(err.Error())+".")
		return diags
	}
	defer func() { _ = dir.Close() }()
	target := account.authorizedKeysPath()

	unlock, err := lockFileForEdit(target)
	if err != nil {
		diags.AddError("Locking file", capitalize(err.Error())+".")
		return diags
	}
	defer unlock()

	data, snap, err := dir.readAuthorizedKeys()
	if err != nil {
		diags.AddError("Reading authorized_keys", capitalize(err.Error())+".")
		return diags
	}
	text := parseTextFile(data)
	changed, hint := false, -1
	if oldMatch != nil {
		changed, hint = removeAuthorizedKey(text, oldMatch)
	}
	line := spec.line()
	if ensureAuthorizedKey(text, match, line, hint) {
		changed = true
	}
	if changed || snap == nil || snap.mode != authorizedKeysFileMode || snap.uid != account.uid || snap.gid != account.gid {
		if err := dir.writeAuthorizedKeys(account, text.bytes(), snap); err != nil {
			diags.AddError("Writing authorized_keys", capitalize(err.Error())+".")
			return diags
		}
	}

	plan.Fingerprint = types.StringValue(spec.fingerprint())
	plan.Line = types.StringValue(line)
	plan.Path = types.StringValue(target)
	plan.ID = types.StringValue(sshKeyID(plan.User.ValueString(), spec.fingerprint()))
	return diags
}

func (r *sshAuthorizedKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sshAuthorizedKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := r.refresh(ctx, &state)
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

// refresh updates m from the user's authorized_keys file. It reports
// found=false if the user, their home directory, ~/.ssh, the file or the
// key is missing. After import, m holds only the user and the fingerprint,
// and key, options and comment are read from the file.
func (r *sshAuthorizedKeyResource) refresh(ctx context.Context, m *sshAuthorizedKeyModel) (found bool, diags diag.Diagnostics) {
	imported := m.Key.IsNull() || m.Key.ValueString() == ""
	var match sshKeyMatcher
	var keyComment string
	if imported {
		fp := m.Fingerprint.ValueString()
		if err := validateSSHKeyFingerprint(fp); err != nil {
			diags.AddError("Invalid state", capitalize(err.Error())+".")
			return false, diags
		}
		match = matchSSHKeyFingerprint(fp)
	} else {
		pub, comment, err := parseSSHPublicKey(m.Key.ValueString())
		if err != nil {
			diags.AddError("Invalid state", capitalize(err.Error())+".")
			return false, diags
		}
		match, keyComment = matchSSHKey(pub), comment
	}

	account, err := r.cfg.lookupAccount(m.User.ValueString())
	if errors.Is(err, errSSHUserNotFound) {
		return false, diags
	}
	if err != nil {
		diags.AddAttributeError(path.Root("user"), "Looking up user", capitalize(err.Error())+".")
		return false, diags
	}
	dir, err := openSSHDir(account, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, diags
	}
	if err != nil {
		diags.AddError("Opening ~/.ssh", capitalize(err.Error())+".")
		return false, diags
	}
	defer func() { _ = dir.Close() }()
	data, _, err := dir.readAuthorizedKeys()
	if err != nil {
		diags.AddError("Reading authorized_keys", capitalize(err.Error())+".")
		return false, diags
	}
	lines := parseTextFile(data).lines
	indices, first := findAuthorizedKey(lines, match)
	if len(indices) == 0 {
		return false, diags
	}

	if imported {
		m.Key = types.StringValue(renderAuthorizedKey(first.pub, nil, first.comment))
		m.Comment = types.StringNull()
		m.Options = types.ListNull(types.StringType)
		if len(first.options) > 0 {
			m.Options = stringList(first.options)
		}
	} else {
		want := keyComment
		if !m.Comment.IsNull() {
			want = m.Comment.ValueString()
		}
		if first.comment != want {
			m.Comment = types.StringValue(first.comment)
		}
		var current []string
		if !m.Options.IsNull() && !m.Options.IsUnknown() {
			diags.Append(m.Options.ElementsAs(ctx, &current, false)...)
		}
		// A null list and an empty one are equal here.
		if !slices.Equal(first.options, current) {
			m.Options = stringList(first.options)
		}
	}

	matched := make([]string, len(indices))
	for i, idx := range indices {
		// line is compared with the planned one, which has no CR.
		matched[i] = strings.ToValidUTF8(strings.TrimSuffix(lines[idx], "\r"), "�")
	}
	fp := sshKeyFingerprint(first.pub)
	m.Line = types.StringValue(strings.Join(matched, "\n"))
	m.Fingerprint = types.StringValue(fp)
	m.Path = types.StringValue(account.authorizedKeysPath())
	m.ID = types.StringValue(sshKeyID(m.User.ValueString(), fp))
	return true, diags
}

// stringList converts values to a list value; an empty slice gives an
// empty list.
func stringList(values []string) types.List {
	elems := make([]attr.Value, len(values))
	for i, v := range values {
		elems[i] = types.StringValue(strings.ToValidUTF8(v, "�"))
	}
	return types.ListValueMust(types.StringType, elems)
}

func (r *sshAuthorizedKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sshAuthorizedKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var match sshKeyMatcher
	if pub, _, err := parseSSHPublicKey(state.Key.ValueString()); err == nil {
		match = matchSSHKey(pub)
	} else if err := validateSSHKeyFingerprint(state.Fingerprint.ValueString()); err == nil {
		match = matchSSHKeyFingerprint(state.Fingerprint.ValueString())
	} else {
		resp.Diagnostics.AddError("Invalid state", "The state holds neither a valid key nor a valid fingerprint.")
		return
	}
	// State is not validated by the schema.
	if err := validateAccountName(state.User.ValueString()); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}

	account, err := r.cfg.lookupAccount(state.User.ValueString())
	if errors.Is(err, errSSHUserNotFound) {
		return // The key went with the user.
	}
	if err != nil {
		resp.Diagnostics.AddError("Looking up user", capitalize(err.Error())+".")
		return
	}
	dir, err := openSSHDir(account, false)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Opening ~/.ssh", capitalize(err.Error())+".")
		return
	}
	defer func() { _ = dir.Close() }()

	unlock, err := lockFileForEdit(account.authorizedKeysPath())
	if err != nil {
		resp.Diagnostics.AddError("Locking file", capitalize(err.Error())+".")
		return
	}
	defer unlock()

	data, snap, err := dir.readAuthorizedKeys()
	if err != nil {
		resp.Diagnostics.AddError("Reading authorized_keys", capitalize(err.Error())+".")
		return
	}
	if snap == nil {
		return
	}
	text := parseTextFile(data)
	if changed, _ := removeAuthorizedKey(text, match); !changed {
		return
	}
	if err := dir.writeAuthorizedKeys(account, text.bytes(), snap); err != nil {
		resp.Diagnostics.AddError("Writing authorized_keys", capitalize(err.Error())+".")
	}
}

// ImportState accepts "<user>:<fingerprint>", with the fingerprint as
// printed by ssh-keygen -l, such as "alice:SHA256:47DEQpj8...".
func (r *sshAuthorizedKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	user, fp, ok := strings.Cut(req.ID, ":")
	if !ok {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must have the form <user>:<fingerprint>, such as \"alice:SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU\", got %q.", req.ID))
		return
	}
	if err := validateAccountName(user); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+".")
		return
	}
	if err := validateSSHKeyFingerprint(fp); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", capitalize(err.Error())+". Print it with ssh-keygen -lf <file>.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user"), user)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("fingerprint"), fp)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), sshKeyID(user, fp))...)
}
