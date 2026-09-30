package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*groupResource)(nil)
	_ resource.ResourceWithImportState    = (*groupResource)(nil)
	_ resource.ResourceWithValidateConfig = (*groupResource)(nil)
)

// groupFile is the group database read by the resource. It is a variable so
// unit tests can point it at a fixture.
var groupFile = "/etc/group"

func NewGroupResource() resource.Resource { return &groupResource{} }

type groupResource struct{}

type groupModel struct {
	Name    types.String `tfsdk:"name"`
	GID     types.Int64  `tfsdk:"gid"`
	System  types.Bool   `tfsdk:"system"`
	Members types.Set    `tfsdk:"members"`
	ID      types.String `tfsdk:"id"`
}

// groupEntry is one parsed line of /etc/group.
type groupEntry struct {
	Name    string
	GID     int64
	Members []string
}

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a local group by shelling out to `groupadd` (create), `groupmod` (gid changes), `gpasswd -M` (member list) and `groupdel` (delete). " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Group name. Changing this forces a new resource. " +
					"Must consist of letters, digits, `_`, `.` and `-`, must not start with `-` or `.`, may end in `$`, must not be purely numeric and is at most 32 characters long.",
				Validators:    []validator.String{accountName()},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"gid": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Numeric group ID. Assigned by the system if unset; changing it runs `groupmod -g`. " +
					"Note that `groupmod` does not re-own files that belong to the old gid.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"system": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Create as a system group (`groupadd -r`), which assigns a gid from the system range. " +
					"Changing this between `true` and `false` forces a new resource.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplaceIf(systemRequiresReplace,
						"Changing system recreates the group.",
						"Changing `system` recreates the group."),
				},
			},
			"members": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Usernames that belong to the group. " +
					"When set, the member list is replaced with exactly these users (`gpasswd -M`); an empty set removes all members. " +
					"When unset, membership is not managed or compared. Every user must already exist.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// systemRequiresReplace replaces the group when system flips between true and
// false. A null state value means the flag is unknown (the group was
// imported), so adopting whatever the configuration says must not destroy it.
func systemRequiresReplace(_ context.Context, req planmodifier.BoolRequest, resp *boolplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	resp.RequiresReplace = req.StateValue.ValueBool() != req.PlanValue.ValueBool()
}

func (r *groupResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var members types.Set
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("members"), &members)...)
	if resp.Diagnostics.HasError() || members.IsNull() || members.IsUnknown() {
		return
	}
	for _, v := range members.Elements() {
		s, ok := v.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			continue
		}
		if err := validateAccountName(s.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("members"), "Invalid member", capitalize(err.Error())+".")
		}
	}
}

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()
	var args []string
	if !plan.GID.IsNull() && !plan.GID.IsUnknown() {
		args = append(args, "-g", strconv.FormatInt(plan.GID.ValueInt64(), 10))
	}
	if plan.System.ValueBool() {
		args = append(args, "-r")
	}
	args = append(args, "--", name)

	if err := runCmd("groupadd", args...); err != nil {
		resp.Diagnostics.AddError("groupadd failed", err.Error())
		return
	}

	if !plan.Members.IsNull() {
		if err := setGroupMembers(ctx, name, plan.Members); err != nil {
			// The group exists now; record it so Terraform can retry the
			// membership change or destroy it instead of leaking it.
			resp.Diagnostics.AddError("Setting group members", err.Error())
			r.saveAfterPartialWrite(ctx, &plan, resp)
			return
		}
	}

	r.refresh(&plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// saveAfterPartialWrite stores whatever the group database now says about a
// group whose creation only partly succeeded. Any error doing so is secondary
// to the one already reported and is dropped; the resource is then simply not
// recorded.
func (r *groupResource) saveAfterPartialWrite(ctx context.Context, m *groupModel, resp *resource.CreateResponse) {
	var d diag.Diagnostics
	r.refresh(m, &d)
	if d.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	entry, err := lookupGroupEntry(state.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading group database", err.Error())
		return
	}
	if entry == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	r.apply(entry, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := state.Name.ValueString()
	if !plan.GID.IsNull() && !plan.GID.IsUnknown() && !plan.GID.Equal(state.GID) {
		gid := strconv.FormatInt(plan.GID.ValueInt64(), 10)
		if err := runCmd("groupmod", "-g", gid, "--", name); err != nil {
			resp.Diagnostics.AddError("groupmod failed", err.Error())
			return
		}
	}
	// A null plan means membership is no longer managed: leave it alone.
	if !plan.Members.IsNull() && !plan.Members.Equal(state.Members) {
		if err := setGroupMembers(ctx, name, plan.Members); err != nil {
			resp.Diagnostics.AddError("Setting group members", err.Error())
			return
		}
	}

	r.refresh(&plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if err := runCmd("groupdel", "--", name); err != nil {
		// Already gone counts as deleted.
		if entry, lookupErr := lookupGroupEntry(name); lookupErr == nil && entry == nil {
			return
		}
		resp.Diagnostics.AddError("groupdel failed", err.Error())
	}
}

// ImportState imports a group by name. Membership is only adopted when the
// group has members, so importing a memberless group into a configuration
// without "members" yields an empty plan.
func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateAccountName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Expected a group name: %s.", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)

	// A missing group is reported by the subsequent Read, which removes it.
	entry, err := lookupGroupEntry(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Reading group database", err.Error())
		return
	}
	if entry != nil && len(entry.Members) > 0 {
		set, d := stringSet(entry.Members)
		resp.Diagnostics.Append(d...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("members"), set)...)
	}
}

// refresh re-reads the group after a write and copies it into m.
func (r *groupResource) refresh(m *groupModel, diags *diag.Diagnostics) {
	entry, err := lookupGroupEntry(m.Name.ValueString())
	if err != nil {
		diags.AddError("Reading group database", err.Error())
		return
	}
	if entry == nil {
		diags.AddError("Group missing after write",
			fmt.Sprintf("Group %q is not in %s.", m.Name.ValueString(), groupFile))
		return
	}
	r.apply(entry, m, diags)
}

// apply copies the observed group into m. Members are only reported when
// managed, so unmanaged membership never shows up as a diff.
func (r *groupResource) apply(entry *groupEntry, m *groupModel, diags *diag.Diagnostics) {
	m.ID = types.StringValue(entry.Name)
	m.GID = types.Int64Value(entry.GID)
	if !m.Members.IsNull() {
		set, d := stringSet(entry.Members)
		diags.Append(d...)
		m.Members = set
	}
}

func setGroupMembers(ctx context.Context, name string, members types.Set) error {
	list, err := groupsFromSet(ctx, members)
	if err != nil {
		return err
	}
	sort.Strings(list)
	return runCmd("gpasswd", "-M", strings.Join(list, ","), "--", name)
}

func stringSet(values []string) (types.Set, diag.Diagnostics) {
	elems := make([]attr.Value, 0, len(values))
	for _, v := range values {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValue(types.StringType, elems)
}

// lookupGroupEntry returns the group called name from groupFile, or nil if
// there is none. The file is parsed directly rather than through os/user
// because the latter does not expose the member list.
func lookupGroupEntry(name string) (*groupEntry, error) {
	return scanGroupFile(func(fields []string) bool { return fields[0] == name })
}

// lookupGroupEntryByGID returns the first group with the given gid from
// groupFile, or nil if there is none. Like getgrgid, the first entry wins if
// several groups share a gid.
func lookupGroupEntryByGID(gid int64) (*groupEntry, error) {
	return scanGroupFile(gidMatcher(gid))
}

func scanGroupFile(match func(fields []string) bool) (*groupEntry, error) {
	f, err := os.Open(groupFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return scanGroupEntries(f, match)
}

func findGroupEntry(r io.Reader, name string) (*groupEntry, error) {
	return scanGroupEntries(r, func(fields []string) bool { return fields[0] == name })
}

func findGroupEntryByGID(r io.Reader, gid int64) (*groupEntry, error) {
	return scanGroupEntries(r, gidMatcher(gid))
}

// gidMatcher matches entries by numeric gid. Entries whose gid does not parse
// cannot be the one asked for and are skipped rather than reported.
func gidMatcher(gid int64) func(fields []string) bool {
	return func(fields []string) bool {
		g, err := strconv.ParseInt(fields[2], 10, 64)
		return err == nil && g == gid
	}
}

// scanGroupEntries returns the first entry in r for whose colon-separated
// fields match returns true, or nil if there is none. match is only called
// with exactly four fields.
func scanGroupEntries(r io.Reader, match func(fields []string) bool) (*groupEntry, error) {
	var found *groupEntry
	err := scanGroupLines(r, groupFile, func(lineNo int, fields []string) (bool, error) {
		if !match(fields) {
			return true, nil
		}
		e, err := parseGroupFields(groupFile, lineNo, fields)
		if err != nil {
			return false, err
		}
		found = e
		return false, nil
	})
	return found, err
}

// listGroupEntries returns every entry of the group database in r, in file
// order. Unlike the lookups, which skip entries they are not looking for, it
// reports an entry with a malformed gid, because the list would otherwise be
// silently incomplete. file names r in error messages.
func listGroupEntries(r io.Reader, file string) ([]groupEntry, error) {
	entries := []groupEntry{}
	err := scanGroupLines(r, file, func(lineNo int, fields []string) (bool, error) {
		e, err := parseGroupFields(file, lineNo, fields)
		if err != nil {
			return false, err
		}
		entries = append(entries, *e)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// scanGroupLines calls fn with the line number and the four colon-separated
// fields of every entry of the group database in r, until fn returns false
// or an error. Blank lines, comments and NIS compat entries ("+", "-name")
// are skipped; a line without exactly four fields is an error. file names r
// in error messages.
func scanGroupLines(r io.Reader, file string, fn func(lineNo int, fields []string) (bool, error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := sc.Text()
		if line == "" || line[0] == '#' || line[0] == '+' || line[0] == '-' {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 4 {
			return fmt.Errorf("%s:%d: expected 4 colon-separated fields, got %d", file, lineNo, len(fields))
		}
		more, err := fn(lineNo, fields)
		if err != nil || !more {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	return nil
}

// parseGroupFields builds the entry for the four fields of a group line.
func parseGroupFields(file string, lineNo int, fields []string) (*groupEntry, error) {
	gid, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s:%d: invalid gid %q", file, lineNo, fields[2])
	}
	entry := &groupEntry{Name: fields[0], GID: gid, Members: []string{}}
	for _, m := range strings.Split(fields[3], ",") {
		if m = strings.TrimSpace(m); m != "" {
			entry.Members = append(entry.Members, m)
		}
	}
	return entry, nil
}
