package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/user"
	"regexp"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*userDataSource)(nil)

func NewUserDataSource() datasource.DataSource { return &userDataSource{} }

type userDataSource struct{}

type userDataSourceModel struct {
	Name    types.String `tfsdk:"name"`
	UID     types.Int64  `tfsdk:"uid"`
	GID     types.Int64  `tfsdk:"gid"`
	Home    types.String `tfsdk:"home"`
	Shell   types.String `tfsdk:"shell"`
	Comment types.String `tfsdk:"comment"`
	Groups  types.Set    `tfsdk:"groups"`
	ID      types.String `tfsdk:"id"`
}

// maxAccountID is the largest valid uid or gid. uid_t and gid_t are 32 bits
// wide and (uid_t)-1 is reserved to mean "no change" by chown(2).
const maxAccountID = math.MaxUint32 - 1

// lookupNameValidator accepts any name that can appear in the passwd or group
// database, which is more than the resources allow to be created (directory
// services may provide names such as "DOMAIN\user" or "user@example.com").
// A leading dash is refused so the name can never be taken for an option.
func lookupNameValidator() validator.String {
	return stringvalidator.RegexMatches(regexp.MustCompile(`^[^-:,\s\x00-\x1f\x7f][^:,\s\x00-\x1f\x7f]*$`),
		"must be non-empty, must not start with `-` and must not contain `:`, `,`, whitespace or control characters")
}

func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing user account by name or by uid. " +
			"Exactly one of `name` and `uid` must be set; the other is filled in from the account that was found.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Username to look up. Conflicts with `uid`. " +
					"When looking up by `uid`, the name of the account that was found.",
				Validators: []validator.String{
					lookupNameValidator(),
					stringvalidator.ExactlyOneOf(path.MatchRoot("uid")),
				},
			},
			"uid": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Numeric user ID to look up. Conflicts with `name`. " +
					"When looking up by `name`, the uid of the account that was found. " +
					"If several accounts share a uid, the first one in the user database is returned.",
				Validators: []validator.Int64{
					int64validator.Between(0, maxAccountID),
					int64validator.ExactlyOneOf(path.MatchRoot("name")),
				},
			},
			"gid": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Numeric ID of the user's primary group.",
			},
			"home": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Home directory.",
			},
			"shell": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Login shell. An empty string if the passwd entry leaves it empty, which login programs treat as `/bin/sh`.",
			},
			"comment": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The complete GECOS comment field, including any comma-separated parts after the full name " +
					"(for example `\"Alice Example,Room 42,,\"`). An empty string if the field is empty.",
			},
			"groups": schema.SetAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Names of the user's supplementary groups, that is every group the user is listed as a member of, except the primary group. " +
					"Groups without a name are omitted.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Data source identifier (equal to `name`).",
			},
		},
	}
}

func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg userDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var (
		u   *user.User
		err error
	)
	switch {
	case !cfg.Name.IsNull():
		u, err = user.Lookup(cfg.Name.ValueString())
	case !cfg.UID.IsNull():
		u, err = user.LookupId(strconv.FormatInt(cfg.UID.ValueInt64(), 10))
	default:
		// Config validation already enforces this; re-check as defense in depth.
		resp.Diagnostics.AddError("Missing lookup key", "Exactly one of name and uid must be set.")
		return
	}
	if err != nil {
		var unknownName user.UnknownUserError
		var unknownID user.UnknownUserIdError
		switch {
		case errors.As(err, &unknownName):
			resp.Diagnostics.AddAttributeError(path.Root("name"), "User not found",
				fmt.Sprintf("No user named %q exists.", cfg.Name.ValueString()))
		case errors.As(err, &unknownID):
			resp.Diagnostics.AddAttributeError(path.Root("uid"), "User not found",
				fmt.Sprintf("No user with uid %d exists.", cfg.UID.ValueInt64()))
		default:
			resp.Diagnostics.AddError("Looking up user", err.Error())
		}
		return
	}

	uid, err := strconv.ParseInt(u.Uid, 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Looking up user", fmt.Sprintf("User %q has a non-numeric uid %q.", u.Username, u.Uid))
		return
	}
	gid, err := strconv.ParseInt(u.Gid, 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Looking up user", fmt.Sprintf("User %q has a non-numeric gid %q.", u.Username, u.Gid))
		return
	}
	entry, err := readPasswdEntry(u.Username)
	if err != nil {
		resp.Diagnostics.AddError("Reading passwd entry", err.Error())
		return
	}
	groups, err := supplementaryGroupNames(u)
	if err != nil {
		resp.Diagnostics.AddError("Listing supplementary groups", fmt.Sprintf("User %q: %s.", u.Username, err))
		return
	}
	groupSet, diags := stringSet(groups)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state := userDataSourceModel{
		Name:    types.StringValue(u.Username),
		UID:     types.Int64Value(uid),
		GID:     types.Int64Value(gid),
		Home:    types.StringValue(u.HomeDir),
		Shell:   types.StringValue(entry.Shell),
		Comment: types.StringValue(entry.Comment),
		Groups:  groupSet,
		ID:      types.StringValue(u.Username),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
