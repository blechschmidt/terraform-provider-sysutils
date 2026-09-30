package provider

import (
	"bytes"
	"context"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource                   = (*groupsDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*groupsDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*groupsDataSource)(nil)
)

func NewGroupsDataSource() datasource.DataSource { return &groupsDataSource{} }

type groupsDataSource struct{ rootedDataSource }

type groupsDataSourceModel struct {
	NameRegex types.String `tfsdk:"name_regex"`
	GIDMin    types.Int64  `tfsdk:"gid_min"`
	GIDMax    types.Int64  `tfsdk:"gid_max"`
	Member    types.String `tfsdk:"member"`
	Groups    types.List   `tfsdk:"groups"`
	Names     types.List   `tfsdk:"names"`
	ID        types.String `tfsdk:"id"`
}

var groupsListEntryType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"name":    types.StringType,
	"gid":     types.Int64Type,
	"members": types.SetType{ElemType: types.StringType},
}}

func (d *groupsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_groups"
}

func (d *groupsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the groups in `/etc/group`, optionally filtered by name, gid range and member. " +
			"With the provider's `root_dir` set, `/etc/group` is read below `root_dir`.",
		Attributes: map[string]schema.Attribute{
			"name_regex": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: nameRegexDescription,
				Validators:          []validator.String{regexpString()},
			},
			"gid_min": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Only return groups whose gid is at least this value.",
				Validators:          []validator.Int64{int64validator.Between(0, maxAccountID)},
			},
			"gid_max": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Only return groups whose gid is at most this value. Must not be less than `gid_min`.",
				Validators:          []validator.Int64{int64validator.Between(0, maxAccountID)},
			},
			"member": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only return groups that list this username as a member in `/etc/group`. " +
					"Groups that are only the user's primary group are not returned.",
				Validators: []validator.String{lookupNameValidator()},
			},
			"groups": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The matching groups, in the order of `/etc/group`. " +
					"Entries that share a name or gid are all listed.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Group name.",
						},
						"gid": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Numeric group ID.",
						},
						"members": schema.SetAttribute{
							Computed:    true,
							ElementType: types.StringType,
							MarkdownDescription: "Usernames listed as members of the group. " +
								"Users whose primary group this is are not included unless they are also listed explicitly.",
						},
					},
				},
			},
			"names": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The names of the matching groups, in the same order as `groups`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (always `/etc/group`).",
			},
		},
	}
}

func (d *groupsDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var m groupsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateIDRange(&resp.Diagnostics, "gid", m.GIDMin, m.GIDMax)
}

func (d *groupsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m groupsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	nameRE, ok := compileNameRegex(&resp.Diagnostics, m.NameRegex)
	if !ok {
		return
	}

	data, err := readRootedFile(d.root(), groupFile, maxAccountDBSize)
	if err != nil {
		resp.Diagnostics.AddError("Reading group database", capitalize(err.Error())+".")
		return
	}
	entries, err := listGroupEntries(bytes.NewReader(data), groupFile)
	if err != nil {
		resp.Diagnostics.AddError("Reading group database", capitalize(err.Error())+".")
		return
	}

	groups := []attr.Value{}
	names := []attr.Value{}
	for _, e := range entries {
		if (nameRE != nil && !nameRE.MatchString(e.Name)) ||
			!idInRange(e.GID, m.GIDMin, m.GIDMax) ||
			(!m.Member.IsNull() && !slices.Contains(e.Members, m.Member.ValueString())) {
			continue
		}
		members, diags := stringSet(e.Members)
		resp.Diagnostics.Append(diags...)
		obj, diags := types.ObjectValue(groupsListEntryType.AttrTypes, map[string]attr.Value{
			"name":    types.StringValue(e.Name),
			"gid":     types.Int64Value(e.GID),
			"members": members,
		})
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		groups = append(groups, obj)
		names = append(names, types.StringValue(e.Name))
	}

	var diags diag.Diagnostics
	m.Groups, diags = types.ListValue(groupsListEntryType, groups)
	resp.Diagnostics.Append(diags...)
	m.Names, diags = types.ListValue(types.StringType, names)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("/etc/group")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
