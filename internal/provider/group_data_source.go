package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*groupDataSource)(nil)

func NewGroupDataSource() datasource.DataSource { return &groupDataSource{} }

type groupDataSource struct{}

type groupDataSourceModel struct {
	Name    types.String `tfsdk:"name"`
	GID     types.Int64  `tfsdk:"gid"`
	Members types.Set    `tfsdk:"members"`
	ID      types.String `tfsdk:"id"`
}

func (d *groupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (d *groupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing group in `/etc/group` by name or by gid. " +
			"Exactly one of `name` and `gid` must be set; the other is filled in from the group that was found.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Group name to look up. Conflicts with `gid`. " +
					"When looking up by `gid`, the name of the group that was found.",
				Validators: []validator.String{
					lookupNameValidator(),
					stringvalidator.ExactlyOneOf(path.MatchRoot("gid")),
				},
			},
			"gid": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Numeric group ID to look up. Conflicts with `name`. " +
					"When looking up by `name`, the gid of the group that was found. " +
					"If several groups share a gid, the first one in `/etc/group` is returned.",
				Validators: []validator.Int64{
					int64validator.Between(0, maxAccountID),
					int64validator.ExactlyOneOf(path.MatchRoot("name")),
				},
			},
			"members": schema.SetAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Usernames listed as members of the group in `/etc/group`. " +
					"Users whose primary group this is are not included unless they are also listed explicitly.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `name`).",
			},
		},
	}
}

func (d *groupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg groupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var (
		entry    *groupEntry
		err      error
		keyPath  path.Path
		notFound string
	)
	switch {
	case !cfg.Name.IsNull():
		entry, err = lookupGroupEntry(cfg.Name.ValueString())
		keyPath, notFound = path.Root("name"), fmt.Sprintf("No group named %q exists in %s.", cfg.Name.ValueString(), groupFile)
	case !cfg.GID.IsNull():
		entry, err = lookupGroupEntryByGID(cfg.GID.ValueInt64())
		keyPath, notFound = path.Root("gid"), fmt.Sprintf("No group with gid %d exists in %s.", cfg.GID.ValueInt64(), groupFile)
	default:
		// Config validation already enforces this; re-check as defense in depth.
		resp.Diagnostics.AddError("Missing lookup key", "Exactly one of name and gid must be set.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading group database", err.Error())
		return
	}
	if entry == nil {
		resp.Diagnostics.AddAttributeError(keyPath, "Group not found", notFound)
		return
	}

	members, diags := stringSet(entry.Members)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state := groupDataSourceModel{
		Name:    types.StringValue(entry.Name),
		GID:     types.Int64Value(entry.GID),
		Members: members,
		ID:      types.StringValue(entry.Name),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
