package provider

import (
	"bytes"
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource                   = (*usersDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*usersDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*usersDataSource)(nil)
)

// passwdFile is the passwd database read by the sysutils_users data source,
// below root_dir. It is a variable so unit tests can point it at a fixture.
var passwdFile = "/etc/passwd"

// maxAccountDBSize bounds how much of /etc/passwd or /etc/group the list data
// sources read, so that a huge or endless file in an untrusted root_dir tree
// cannot exhaust the provider's memory. Real databases are far smaller.
const maxAccountDBSize = 64 << 20

func NewUsersDataSource() datasource.DataSource { return &usersDataSource{} }

type usersDataSource struct{ rootedDataSource }

type usersDataSourceModel struct {
	NameRegex types.String `tfsdk:"name_regex"`
	UIDMin    types.Int64  `tfsdk:"uid_min"`
	UIDMax    types.Int64  `tfsdk:"uid_max"`
	GID       types.Int64  `tfsdk:"gid"`
	Shell     types.String `tfsdk:"shell"`
	Users     types.List   `tfsdk:"users"`
	Names     types.List   `tfsdk:"names"`
	ID        types.String `tfsdk:"id"`
}

var usersListEntryType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"name":    types.StringType,
	"uid":     types.Int64Type,
	"gid":     types.Int64Type,
	"comment": types.StringType,
	"home":    types.StringType,
	"shell":   types.StringType,
}}

// nameRegexDescription documents name_regex of both list data sources.
const nameRegexDescription = "Only return entries whose name matches this [RE2 regular expression](https://github.com/google/re2/wiki/Syntax). " +
	"The expression matches anywhere in the name unless it is anchored, so use `^...$` to match whole names."

func (d *usersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_users"
}

func (d *usersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the user accounts in `/etc/passwd`, optionally filtered by name, uid range, primary group and login shell. " +
			"With the provider's `root_dir` set, `/etc/passwd` is read below `root_dir`.",
		Attributes: map[string]schema.Attribute{
			"name_regex": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: nameRegexDescription,
				Validators:          []validator.String{regexpString()},
			},
			"uid_min": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Only return users whose uid is at least this value.",
				Validators:          []validator.Int64{int64validator.Between(0, maxAccountID)},
			},
			"uid_max": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Only return users whose uid is at most this value. Must not be less than `uid_min`.",
				Validators:          []validator.Int64{int64validator.Between(0, maxAccountID)},
			},
			"gid": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Only return users whose primary group has this gid.",
				Validators:          []validator.Int64{int64validator.Between(0, maxAccountID)},
			},
			"shell": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only return users with exactly this login shell, for example `/bin/bash`. " +
					"An empty string matches entries that leave the shell empty.",
			},
			"users": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The matching users, in the order of `/etc/passwd`. " +
					"Entries that share a name or uid are all listed.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Username.",
						},
						"uid": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Numeric user ID.",
						},
						"gid": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Numeric ID of the user's primary group.",
						},
						"comment": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The complete GECOS comment field. An empty string if the field is empty.",
						},
						"home": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Home directory.",
						},
						"shell": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Login shell. An empty string if the entry leaves it empty, which login programs treat as `/bin/sh`.",
						},
					},
				},
			},
			"names": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The names of the matching users, in the same order as `users`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (always `/etc/passwd`).",
			},
		},
	}
}

func (d *usersDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var m usersDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateIDRange(&resp.Diagnostics, "uid", m.UIDMin, m.UIDMax)
}

func (d *usersDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m usersDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	nameRE, ok := compileNameRegex(&resp.Diagnostics, m.NameRegex)
	if !ok {
		return
	}

	data, err := readRootedFile(d.root(), passwdFile, maxAccountDBSize)
	if err != nil {
		resp.Diagnostics.AddError("Reading passwd database", capitalize(err.Error())+".")
		return
	}
	entries, err := listPasswdEntries(bytes.NewReader(data), passwdFile)
	if err != nil {
		resp.Diagnostics.AddError("Reading passwd database", capitalize(err.Error())+".")
		return
	}

	users := []attr.Value{}
	names := []attr.Value{}
	for _, e := range entries {
		if (nameRE != nil && !nameRE.MatchString(e.name)) ||
			!idInRange(e.uid, m.UIDMin, m.UIDMax) ||
			(!m.GID.IsNull() && e.gid != m.GID.ValueInt64()) ||
			(!m.Shell.IsNull() && e.shell != m.Shell.ValueString()) {
			continue
		}
		obj, diags := types.ObjectValue(usersListEntryType.AttrTypes, map[string]attr.Value{
			"name":    types.StringValue(e.name),
			"uid":     types.Int64Value(e.uid),
			"gid":     types.Int64Value(e.gid),
			"comment": types.StringValue(e.gecos),
			"home":    types.StringValue(e.home),
			"shell":   types.StringValue(e.shell),
		})
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		users = append(users, obj)
		names = append(names, types.StringValue(e.name))
	}

	var diags diag.Diagnostics
	m.Users, diags = types.ListValue(usersListEntryType, users)
	resp.Diagnostics.Append(diags...)
	m.Names, diags = types.ListValue(types.StringType, names)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("/etc/passwd")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// validateIDRange reports a range whose minimum is above its maximum. kind is
// "uid" or "gid" and prefixes the attribute names.
func validateIDRange(diags *diag.Diagnostics, kind string, lo, hi types.Int64) {
	if lo.IsNull() || lo.IsUnknown() || hi.IsNull() || hi.IsUnknown() {
		return
	}
	if lo.ValueInt64() > hi.ValueInt64() {
		diags.AddAttributeError(path.Root(kind+"_max"), "Invalid "+kind+" range",
			fmt.Sprintf("%s_max (%d) must not be less than %s_min (%d).", kind, hi.ValueInt64(), kind, lo.ValueInt64()))
	}
}

// idInRange reports whether id lies within the optional, inclusive bounds.
func idInRange(id int64, lo, hi types.Int64) bool {
	return (lo.IsNull() || id >= lo.ValueInt64()) && (hi.IsNull() || id <= hi.ValueInt64())
}

// compileNameRegex compiles name_regex. The validator has already rejected
// invalid expressions; they are checked again as defense in depth. A nil
// expression means no filter.
func compileNameRegex(diags *diag.Diagnostics, v types.String) (*regexp.Regexp, bool) {
	if v.IsNull() {
		return nil, true
	}
	re, err := regexp.Compile(v.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("name_regex"), "Invalid regular expression", capitalize(err.Error())+".")
		return nil, false
	}
	return re, true
}
