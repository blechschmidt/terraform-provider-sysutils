package provider

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
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
	_ resource.Resource                   = (*firewallRuleResource)(nil)
	_ resource.ResourceWithConfigure      = (*firewallRuleResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*firewallRuleResource)(nil)
	_ resource.ResourceWithValidateConfig = (*firewallRuleResource)(nil)
	_ resource.ResourceWithImportState    = (*firewallRuleResource)(nil)
)

// firewallLockTimeout bounds how long a change waits for the firewall lock
// held by another resource or provider process.
const firewallLockTimeout = 5 * time.Minute

func NewFirewallRuleResource() resource.Resource { return &firewallRuleResource{} }

type firewallRuleResource struct {
	hostOnlyResource
	cfg *firewallConfig
}

type firewallRuleModel struct {
	Name             types.String `tfsdk:"name"`
	Family           types.String `tfsdk:"family"`
	Chain            types.String `tfsdk:"chain"`
	Protocol         types.String `tfsdk:"protocol"`
	Source           types.String `tfsdk:"source"`
	Destination      types.String `tfsdk:"destination"`
	SourcePorts      types.Set    `tfsdk:"source_ports"`
	DestinationPorts types.Set    `tfsdk:"destination_ports"`
	InInterface      types.String `tfsdk:"in_interface"`
	OutInterface     types.String `tfsdk:"out_interface"`
	Action           types.String `tfsdk:"action"`
	Comment          types.String `tfsdk:"comment"`
	Backend          types.String `tfsdk:"backend"`
	ActiveBackend    types.String `tfsdk:"active_backend"`
	ID               types.String `tfsdk:"id"`
}

func (r *firewallRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall_rule"
}

func quotedList(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = "`\"" + v + "\"`"
	}
	return strings.Join(q, ", ")
}

func (r *firewallRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	portSet := func(desc string) schema.SetAttribute {
		return schema.SetAttribute{
			Optional:    true,
			ElementType: types.StringType,
			MarkdownDescription: desc + " Each element is a port, such as `\"22\"`, or a range, such as `\"8000-8100\"`. " +
				"Requires `protocol` `\"tcp\"`, `\"udp\"` or `\"sctp\"`. Ports must not overlap, and there can be at most 15, where a range counts as two, the limit of iptables' `multiport` match. " +
				"Omit it to match any port; an empty set is not allowed.",
			Validators: []validator.Set{
				setvalidator.SizeAtLeast(1),
				setvalidator.ValueStringsAre(stringCheck("port", validatePort)),
			},
		}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one firewall rule in the `filter` table, with nftables or iptables. " +
			"With nftables, the rule is added to the provider's own table `inet " + nftTable + "`; with iptables, to the built-in `INPUT`, `OUTPUT` or `FORWARD` chain. " +
			"The rule is tagged with a comment that holds `name`, by which it is found again, so rules added by hand or by other tools are never changed. " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Unique name of the rule, stored in its comment as `" + firewallTagPrefix + "<name>`. " +
					"1 to 48 letters, digits, `_`, `.` or `-`, starting with a letter or digit. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("rule name", validateFirewallName)},
			},
			"family": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(firewallFamilyInet),
				MarkdownDescription: "Address family the rule applies to: " + quotedList(firewallFamilies) + ". " +
					"`\"inet\"` matches IPv4 and IPv6; with iptables it adds one rule with `iptables` and one with `ip6tables`. " +
					"`source`, `destination` and the ICMP protocols require `\"ipv4\"` or `\"ipv6\"`. Defaults to `\"inet\"`.",
				Validators: []validator.String{stringvalidator.OneOf(firewallFamilies...)},
			},
			"chain": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Chain of the rule: " + quotedList(firewallChains) + ", for packets addressed to this host, sent by it, and routed through it. " +
					"Changing it moves the rule.",
				Validators: []validator.String{stringvalidator.OneOf(firewallChains...)},
			},
			"protocol": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(firewallProtoAll),
				MarkdownDescription: "Layer 4 protocol to match: " + quotedList(firewallProtocols) + ". " +
					"`\"icmp\"` requires family `\"ipv4\"`, `\"icmpv6\"` family `\"ipv6\"`. Defaults to `\"all\"`, any protocol.",
				Validators: []validator.String{stringvalidator.OneOf(firewallProtocols...)},
			},
			"source": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Source address or CIDR prefix to match, such as `\"10.0.0.0/8\"` or `\"2001:db8::1\"`. " +
					"The prefix must not have host bits set. Requires `family` `\"ipv4\"` or `\"ipv6\"`, matching the address. Omit it to match any source.",
				Validators: []validator.String{stringCheck("source address", validateFirewallAddress)},
			},
			"destination": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Destination address or CIDR prefix to match, as for `source`. Omit it to match any destination.",
				Validators:          []validator.String{stringCheck("destination address", validateFirewallAddress)},
			},
			"source_ports":      portSet("Source ports to match."),
			"destination_ports": portSet("Destination ports to match, such as the port of a service running on this host."),
			"in_interface": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Name of the interface the packet arrived on, such as `\"eth0\"`. Not allowed in the `\"output\"` chain. " +
					"Wildcards are not supported.",
				Validators: []validator.String{stringCheck("interface name", validateInterfaceName)},
			},
			"out_interface": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Name of the interface the packet leaves through. Not allowed in the `\"input\"` chain. Wildcards are not supported.",
				Validators:          []validator.String{stringCheck("interface name", validateInterfaceName)},
			},
			"action": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What to do with matching packets: " + quotedList(firewallActions) + ". " +
					"`\"reject\"` answers with an ICMP port-unreachable error; `\"drop\"` discards the packet silently.",
				Validators: []validator.String{stringvalidator.OneOf(firewallActions...)},
			},
			"comment": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Free-text comment, stored in the rule's comment after the name. " +
					"1 to 64 letters, digits, single spaces or characters of `_.,:/@+=()-`.",
				Validators: []validator.String{stringCheck("comment", validateFirewallComment)},
			},
			"backend": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(firewallBackendAuto),
				MarkdownDescription: "Firewall backend: " + quotedList(firewallBackends) + ". " +
					"`\"auto\"` uses nftables if the `nft` command works, and iptables otherwise; the choice is made when the rule is created and kept afterwards (see `active_backend`). " +
					"Changing it to another backend moves the rule. Defaults to `\"auto\"`.",
				Validators: []validator.String{stringvalidator.OneOf(firewallBackends...)},
			},
			"active_backend": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The backend that holds the rule: `\"nftables\"` or `\"iptables\"`.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *firewallRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.firewall
		r.fsRoot = data.root
	}
}

// ValidateConfig checks the combinations of attributes that no backend
// supports, once all of them are known.
func (r *firewallRuleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m firewallRuleModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, v := range []interface{ IsUnknown() bool }{m.Name, m.Family, m.Chain, m.Protocol, m.Source, m.Destination,
		m.SourcePorts, m.DestinationPorts, m.InInterface, m.OutInterface, m.Action, m.Comment} {
		if v.IsUnknown() {
			return
		}
	}
	if m.Family.IsNull() {
		m.Family = types.StringValue(firewallFamilyInet)
	}
	if m.Protocol.IsNull() {
		m.Protocol = types.StringValue(firewallProtoAll)
	}
	// The attribute validators check each port; overlaps and the number of
	// ports are only visible here.
	for _, ps := range []struct {
		attr string
		set  types.Set
	}{{"source_ports", m.SourcePorts}, {"destination_ports", m.DestinationPorts}} {
		var list []string
		if ps.set.IsNull() || ps.set.ElementsAs(ctx, &list, false).HasError() {
			continue
		}
		if slices.ContainsFunc(list, func(p string) bool { return validatePort(p) != nil }) {
			continue
		}
		if _, err := parsePorts(list); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root(ps.attr), "Invalid ports", capitalize(err.Error())+".")
		}
	}
	if resp.Diagnostics.HasError() || m.Name.IsNull() || m.Chain.IsNull() || m.Action.IsNull() {
		return
	}
	rule, diags := firewallRuleFromModel(ctx, m)
	if diags.HasError() {
		// The attribute validators report these.
		return
	}
	if err := rule.validate(); err != nil {
		resp.Diagnostics.AddError("Invalid firewall rule", capitalize(err.Error())+".")
	}
}

func (r *firewallRuleResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	r.hostOnlyResource.ModifyPlan(ctx, req, resp)
	if resp.Diagnostics.HasError() || req.Plan.Raw.IsNull() {
		return
	}
	var plan firewallRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	active := types.StringUnknown()
	switch {
	case plan.Backend.IsUnknown():
	case plan.Backend.ValueString() != firewallBackendAuto:
		active = plan.Backend
	case !req.State.Raw.IsNull():
		// "auto" sticks to the backend the rule was created with.
		var state firewallRuleModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if !state.ActiveBackend.IsNull() && !state.ActiveBackend.IsUnknown() {
			active = state.ActiveBackend
		}
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("active_backend"), active)...)
}

// firewallRuleFromModel converts m, whose attributes must be known, to a
// firewallRule. It parses addresses and ports but does not validate the
// combination.
func firewallRuleFromModel(ctx context.Context, m firewallRuleModel) (firewallRule, diag.Diagnostics) {
	var diags diag.Diagnostics
	rule := firewallRule{
		name:     m.Name.ValueString(),
		family:   m.Family.ValueString(),
		chain:    m.Chain.ValueString(),
		protocol: m.Protocol.ValueString(),
		inIface:  m.InInterface.ValueString(),
		outIface: m.OutInterface.ValueString(),
		action:   m.Action.ValueString(),
		comment:  m.Comment.ValueString(),
	}
	var err error
	if !m.Source.IsNull() {
		if rule.source, err = parseFirewallAddress(m.Source.ValueString()); err != nil {
			diags.AddAttributeError(path.Root("source"), "Invalid source address", capitalize(err.Error())+".")
		}
	}
	if !m.Destination.IsNull() {
		if rule.destination, err = parseFirewallAddress(m.Destination.ValueString()); err != nil {
			diags.AddAttributeError(path.Root("destination"), "Invalid destination address", capitalize(err.Error())+".")
		}
	}
	for _, ps := range []struct {
		attr string
		set  types.Set
		dst  *[]portRange
	}{{"source_ports", m.SourcePorts, &rule.sourcePorts}, {"destination_ports", m.DestinationPorts, &rule.destPorts}} {
		if ps.set.IsNull() {
			continue
		}
		var list []string
		diags.Append(ps.set.ElementsAs(ctx, &list, false)...)
		if diags.HasError() {
			continue
		}
		if *ps.dst, err = parsePorts(list); err != nil {
			diags.AddAttributeError(path.Root(ps.attr), "Invalid ports", capitalize(err.Error())+".")
		}
	}
	return rule, diags
}

// setModelFromRule records rule, as found on the host, in m. Addresses and
// port sets that are equal to the ones in m keep their spelling.
func setModelFromRule(ctx context.Context, m *firewallRuleModel, rule firewallRule) diag.Diagnostics {
	var diags diag.Diagnostics
	m.Family = types.StringValue(rule.family)
	m.Chain = types.StringValue(rule.chain)
	m.Protocol = types.StringValue(rule.protocol)
	m.Action = types.StringValue(rule.action)
	optional := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	m.InInterface = optional(rule.inIface)
	m.OutInterface = optional(rule.outIface)
	m.Comment = optional(rule.comment)
	address := func(cur types.String, p netip.Prefix) types.String {
		if !p.IsValid() {
			return types.StringNull()
		}
		if !cur.IsNull() && !cur.IsUnknown() {
			if old, err := parseFirewallAddress(cur.ValueString()); err == nil && old == p {
				return cur
			}
		}
		return types.StringValue(formatFirewallAddress(p))
	}
	m.Source = address(m.Source, rule.source)
	m.Destination = address(m.Destination, rule.destination)
	ports := func(cur types.Set, ports []portRange) types.Set {
		if len(ports) == 0 {
			return types.SetNull(types.StringType)
		}
		want := portStrings(ports)
		if !cur.IsNull() && !cur.IsUnknown() {
			var have []string
			if !cur.ElementsAs(ctx, &have, false).HasError() {
				if old, err := parsePorts(have); err == nil && slices.Equal(portStrings(old), want) {
					return cur
				}
			}
		}
		v, d := types.SetValueFrom(ctx, types.StringType, want)
		diags.Append(d...)
		return v
	}
	m.SourcePorts = ports(m.SourcePorts, rule.sourcePorts)
	m.DestinationPorts = ports(m.DestinationPorts, rule.destPorts)
	return diags
}

// backendFor returns the backend of kind, resolving "auto".
func (r *firewallRuleResource) backendFor(ctx context.Context, kind string) (firewallBackend, error) {
	if kind == firewallBackendAuto || kind == "" {
		var err error
		if kind, err = r.cfg.detect(ctx); err != nil {
			return nil, err
		}
	}
	return r.cfg.backend(kind)
}

// lock takes the firewall lock for a change.
func (r *firewallRuleResource) lock(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, firewallLockTimeout)
	defer cancel()
	return lockFirewall(ctx)
}

func (r *firewallRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan firewallRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	want, diags := firewallRuleFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := want.validate(); err != nil {
		resp.Diagnostics.AddError("Invalid firewall rule", capitalize(err.Error())+".")
		return
	}
	b, err := r.backendFor(ctx, plan.Backend.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("backend"), "Firewall backend not available", capitalize(err.Error())+".")
		return
	}
	unlock, err := r.lock(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Locking the firewall", capitalize(err.Error())+".")
		return
	}
	defer unlock()

	found, err := b.find(ctx, want.name)
	if err != nil {
		resp.Diagnostics.AddError("Listing firewall rules", capitalize(err.Error())+".")
		return
	}
	// Refuse to take over rules that exist already: they may belong to
	// another resource with the same name.
	if !found.empty() {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Firewall rule already exists",
			fmt.Sprintf("The %s backend already has a rule named %q (%s). Import it with \"terraform import\", choose another name, or delete the rule.",
				b.kind(), want.name, found.rules[0].text))
		return
	}
	changed, err := b.apply(ctx, want, found)
	plan.ActiveBackend = types.StringValue(b.kind())
	plan.ID = plan.Name
	if err != nil {
		resp.Diagnostics.AddError("Adding firewall rule", capitalize(err.Error())+".")
		// Record a partly added rule, so that it is tainted and destroy
		// removes what was added.
		if !changed {
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *firewallRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state firewallRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if err := validateFirewallName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	// Import sets only name, backend and id.
	imported := state.Chain.IsNull()

	var kinds []string
	switch {
	case !state.ActiveBackend.IsNull() && !state.ActiveBackend.IsUnknown():
		kinds = []string{state.ActiveBackend.ValueString()}
	case state.Backend.ValueString() == firewallBackendNftables || state.Backend.ValueString() == firewallBackendIptables:
		kinds = []string{state.Backend.ValueString()}
	default:
		// An import with backend "auto" looks in every usable backend.
		for _, k := range []string{firewallBackendNftables, firewallBackendIptables} {
			if r.cfg.usable(ctx, k) {
				kinds = append(kinds, k)
			}
		}
		if len(kinds) == 0 {
			_, err := r.cfg.detect(ctx)
			resp.Diagnostics.AddError("Firewall backend not available", capitalize(err.Error())+".")
			return
		}
	}

	var b firewallBackend
	var found *firewallFound
	for _, kind := range kinds {
		kb, err := r.cfg.backend(kind)
		if err != nil {
			resp.Diagnostics.AddError("Firewall backend not available", capitalize(err.Error())+".")
			return
		}
		f, err := kb.find(ctx, name)
		if err != nil {
			resp.Diagnostics.AddError("Listing firewall rules", capitalize(err.Error())+".")
			return
		}
		if f.empty() {
			continue
		}
		if found != nil {
			resp.Diagnostics.AddError("Firewall rule found in several backends",
				fmt.Sprintf("Both nftables and iptables have a rule named %q. Import it as \"nftables:%s\" or \"iptables:%s\".", name, name, name))
			return
		}
		b, found = kb, f
	}
	if found == nil {
		if imported {
			resp.Diagnostics.AddError("Cannot import firewall rule",
				fmt.Sprintf("No rule with the comment %q was found in the %s backend.", firewallTagPrefix+name, strings.Join(kinds, " or ")))
			return
		}
		resp.State.RemoveResource(ctx)
		return
	}

	state.ActiveBackend = types.StringValue(b.kind())
	state.ID = types.StringValue(name)
	rule, err := found.merged()
	if err != nil {
		if imported {
			resp.Diagnostics.AddError("Cannot import firewall rule", capitalize(err.Error())+".")
			return
		}
		// The rule was changed in a way the attributes cannot express.
		// Clearing action makes the plan show an update, which replaces the
		// rule.
		resp.Diagnostics.AddWarning("Firewall rule changed outside Terraform",
			fmt.Sprintf("%s. The next apply replaces it.", capitalize(err.Error())))
		state.Action = types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	resp.Diagnostics.Append(setModelFromRule(ctx, &state, rule)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *firewallRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state firewallRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	want, diags := firewallRuleFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := want.validate(); err != nil {
		resp.Diagnostics.AddError("Invalid firewall rule", capitalize(err.Error())+".")
		return
	}
	oldKind := state.ActiveBackend.ValueString()
	kind := plan.Backend.ValueString()
	if kind == firewallBackendAuto && oldKind != "" {
		kind = oldKind
	}
	b, err := r.backendFor(ctx, kind)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("backend"), "Firewall backend not available", capitalize(err.Error())+".")
		return
	}
	unlock, err := r.lock(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Locking the firewall", capitalize(err.Error())+".")
		return
	}
	defer unlock()

	// Moving to another backend: remove the rule from the old one first.
	if oldKind != "" && oldKind != b.kind() {
		ob, err := r.cfg.backend(oldKind)
		if err != nil {
			resp.Diagnostics.AddError("Firewall backend not available", fmt.Sprintf("Removing the rule from %s: %s.", oldKind, err))
			return
		}
		found, err := ob.find(ctx, want.name)
		if err == nil {
			err = ob.remove(ctx, found)
		}
		if err != nil {
			resp.Diagnostics.AddError("Removing firewall rule", fmt.Sprintf("Removing the rule from %s: %s.", oldKind, err))
			return
		}
	}
	found, err := b.find(ctx, want.name)
	if err != nil {
		resp.Diagnostics.AddError("Listing firewall rules", capitalize(err.Error())+".")
		return
	}
	if _, err := b.apply(ctx, want, found); err != nil {
		resp.Diagnostics.AddError("Updating firewall rule", capitalize(err.Error())+".")
		return
	}
	plan.ActiveBackend = types.StringValue(b.kind())
	plan.ID = plan.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *firewallRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state firewallRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	if err := validateFirewallName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	kind := state.ActiveBackend.ValueString()
	if kind == "" {
		// Nothing was ever added.
		return
	}
	b, err := r.cfg.backend(kind)
	if err != nil {
		resp.Diagnostics.AddError("Firewall backend not available", capitalize(err.Error())+".")
		return
	}
	unlock, err := r.lock(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Locking the firewall", capitalize(err.Error())+".")
		return
	}
	defer unlock()
	found, err := b.find(ctx, name)
	if err == nil {
		err = b.remove(ctx, found)
	}
	if err != nil {
		resp.Diagnostics.AddError("Removing firewall rule", capitalize(err.Error())+".")
	}
}

// ImportState imports a rule by name, or by "<backend>:<name>" to look in
// one backend only.
func (r *firewallRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	backend, name := firewallBackendAuto, req.ID
	if b, n, ok := strings.Cut(req.ID, ":"); ok {
		if b != firewallBackendNftables && b != firewallBackendIptables {
			resp.Diagnostics.AddError("Invalid import ID",
				fmt.Sprintf("Import ID must be <name> or <backend>:<name>, where backend is nftables or iptables, not %q.", b))
			return
		}
		backend, name = b, n
	}
	if err := validateFirewallName(name); err != nil {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import ID must be <name> or <backend>:<name>: %s.", capitalize(err.Error())))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("backend"), backend)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
}
