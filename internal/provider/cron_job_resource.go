package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
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
	_ resource.Resource                = (*cronJobResource)(nil)
	_ resource.ResourceWithConfigure   = (*cronJobResource)(nil)
	_ resource.ResourceWithImportState = (*cronJobResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*cronJobResource)(nil)
)

func NewCronJobResource() resource.Resource { return &cronJobResource{} }

type cronJobResource struct {
	cfg *cronConfig
}

type cronJobModel struct {
	Name        types.String `tfsdk:"name"`
	Schedule    types.String `tfsdk:"schedule"`
	User        types.String `tfsdk:"user"`
	Command     types.String `tfsdk:"command"`
	Environment types.Map    `tfsdk:"environment"`
	Comment     types.String `tfsdk:"comment"`
	Path        types.String `tfsdk:"path"`
	Content     types.String `tfsdk:"content"`
	Mode        types.String `tfsdk:"mode"`
	Owner       types.String `tfsdk:"owner"`
	Group       types.String `tfsdk:"group"`
	ID          types.String `tfsdk:"id"`
}

func (r *cronJobResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cron_job"
}

func (r *cronJobResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a cron job in its own file in `/etc/cron.d`. " +
			"The file is written atomically with mode `0644` and owned by `root:root`, as cron requires, and removed on destroy. " +
			"Requires root privileges.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name of the file in `/etc/cron.d`, such as `\"backup\"`. " +
					"Only letters, digits, `_` and `-` are allowed: cron ignores files with other characters, such as `.`, in their names. " +
					"Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringCheck("cron job name", validateCronName)},
			},
			"schedule": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "When to run the command: five time fields separated by single spaces " +
					"(minute `0-59`, hour `0-23`, day of month `1-31`, month `1-12` or `jan`-`dec`, day of week `0-7` or `sun`-`sat`, where `0` and `7` are Sunday), such as `\"30 2 * * 1-5\"`, " +
					"or one of `@reboot`, `@yearly`, `@annually`, `@monthly`, `@weekly`, `@daily`, `@midnight` and `@hourly`. " +
					"Each field is `*` or a comma-separated list of values and ranges `a-b`; `*` and ranges may be followed by a step `/n`.",
				Validators: []validator.String{stringCheck("cron schedule", validateCronSchedule)},
			},
			"user": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("root"),
				MarkdownDescription: "User to run the command as. Defaults to `\"root\"`.",
				Validators:          []validator.String{accountName()},
			},
			"command": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Command line, run by `/bin/sh` unless `environment` sets `SHELL`. " +
					"Written to the file verbatim, so a `%` must be escaped as `\\%`: cron turns an unescaped `%` into a newline and passes everything after the first one to the command as standard input. " +
					"Must be a single line of at most 1000 bytes without leading or trailing white space.",
				Validators: []validator.String{stringCheck("cron command", validateCronCommand)},
			},
			"environment": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Environment variables to set for the command, such as `{ MAILTO = \"ops@example.com\" }`, written as `NAME=value` lines before the job. " +
					"`SHELL`, `PATH`, `MAILTO` and, depending on the cron implementation, `CRON_TZ` also configure cron itself. " +
					"Names consist of letters, digits and `_`; values must be single lines, and are quoted as needed.",
				Validators: []validator.Map{
					mapvalidator.KeysAre(stringCheck("environment variable name", validateCronEnvName)),
					mapvalidator.ValueStringsAre(stringCheck("environment variable value", validateCronEnvValue)),
				},
			},
			"comment": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Text written as `#` comment lines at the top of the file, after a line saying that the file is managed by Terraform. " +
					"May span several lines.",
				Validators: []validator.String{stringCheck("comment", validateCronComment)},
			},
			"path": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Path of the file, `/etc/cron.d/<name>`.",
			},
			"content": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "Contents of the file. Refresh reads the file, so any change made outside Terraform, including lines added to the file, shows up as a planned change to this attribute. " +
					"Sensitive, because the command and environment may contain secrets.",
			},
			"mode": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Mode of the file; always `\"0644\"` after apply. A different mode on disk shows up as drift.",
			},
			"owner": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Owner of the file; always `\"root\"` after apply. A different owner on disk shows up as drift.",
			},
			"group": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Group of the file; always the group with GID 0 (`\"root\"`) after apply. A different group on disk shows up as drift.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (equal to `name`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *cronJobResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	data, diags := providerDataFrom(req.ProviderData)
	resp.Diagnostics.Append(diags...)
	if data != nil {
		r.cfg = data.cron
	}
}

// ModifyPlan sets the computed attributes to what apply writes, so that
// refresh reading anything else from the file (other contents, mode or
// ownership) plans an update.
func (r *cronJobResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // Destroy.
	}
	var plan cronJobModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Name.IsUnknown() {
		plan.Path, plan.ID = types.StringUnknown(), types.StringUnknown()
	} else {
		plan.Path = types.StringValue(r.cfg.jobPath(plan.Name.ValueString()))
		plan.ID = plan.Name
	}
	plan.Content = types.StringUnknown()
	if job, known, diags := cronJobFromModel(ctx, &plan); diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	} else if known {
		plan.Content = types.StringValue(string(job.render()))
	}
	uid, gid := r.cfg.owner()
	plan.Mode = types.StringValue(formatMode(cronFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// cronJobFromModel returns the job m describes. known is false if any part
// of it is unknown.
func cronJobFromModel(ctx context.Context, m *cronJobModel) (job cronJob, known bool, diags diag.Diagnostics) {
	for _, v := range []types.String{m.Schedule, m.User, m.Command, m.Comment} {
		if v.IsUnknown() {
			return job, false, diags
		}
	}
	if m.Environment.IsUnknown() {
		return job, false, diags
	}
	var env map[string]types.String
	diags.Append(m.Environment.ElementsAs(ctx, &env, false)...)
	if diags.HasError() {
		return job, false, diags
	}
	job = cronJob{
		schedule:   m.Schedule.ValueString(),
		user:       m.User.ValueString(),
		command:    m.Command.ValueString(),
		env:        make(map[string]string, len(env)),
		comment:    m.Comment.ValueString(),
		hasComment: !m.Comment.IsNull() && m.Comment.ValueString() != "",
	}
	for k, v := range env {
		if v.IsUnknown() {
			return job, false, diags
		}
		job.env[k] = v.ValueString()
	}
	return job, true, diags
}

func (r *cronJobResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan cronJobModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.write(ctx, &plan, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *cronJobResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan cronJobModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.write(ctx, &plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// write renders plan and writes it to the job's file, and sets the computed
// attributes of plan. With create set, an existing file is an error.
func (r *cronJobResource) write(ctx context.Context, plan *cronJobModel, create bool) (diags diag.Diagnostics) {
	name := plan.Name.ValueString()
	if err := validateCronName(name); err != nil {
		diags.AddAttributeError(path.Root("name"), "Invalid name", capitalize(err.Error())+".")
		return diags
	}
	job, known, d := cronJobFromModel(ctx, plan)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	if !known {
		diags.AddError("Unknown values", "All attributes of the cron job must be known at apply time.")
		return diags
	}
	if err := job.validate(); err != nil {
		diags.AddError("Invalid cron job", capitalize(err.Error())+".")
		return diags
	}
	p := r.cfg.jobPath(name)
	uid, gid := r.cfg.owner()
	content := job.render()
	if err := writeCronFile(p, content, uid, gid, create); err != nil {
		if errors.Is(err, errCronFileExists) {
			diags.AddAttributeError(path.Root("name"), "Cron job already exists",
				fmt.Sprintf("%s already exists. Import it with \"terraform import\" instead, or remove the file.", p))
			return diags
		}
		diags.AddError("Writing cron job", fmt.Sprintf("Writing %s: %s.", p, err))
		return diags
	}
	plan.ID = types.StringValue(name)
	plan.Path = types.StringValue(p)
	plan.Content = types.StringValue(string(content))
	plan.Mode = types.StringValue(formatMode(cronFileMode))
	plan.Owner = types.StringValue(uidName(uid))
	plan.Group = types.StringValue(gidName(gid))
	return diags
}

func (r *cronJobResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state cronJobModel
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

// refresh updates m from the job's file. It reports gone if the file does
// not exist.
func (r *cronJobResource) refresh(ctx context.Context, m *cronJobModel) (gone bool, diags diag.Diagnostics) {
	name := m.Name.ValueString()
	// State is not validated by the schema; never read an arbitrary file.
	if err := validateCronName(name); err != nil {
		diags.AddError("Invalid state", capitalize(err.Error())+".")
		return false, diags
	}
	imported := m.Schedule.IsNull()
	p := r.cfg.jobPath(name)
	data, snap, err := readCronFile(p)
	if err != nil {
		diags.AddError("Reading cron job", fmt.Sprintf("Reading %s: %s.", p, err))
		return false, diags
	}
	if snap == nil {
		if imported {
			diags.AddError("Cannot import cron job", fmt.Sprintf("%s does not exist.", p))
		}
		return true, diags
	}

	parsed := parseCronFile(data)
	switch {
	case !parsed.hasJob && imported:
		diags.AddError("Cannot import cron job", fmt.Sprintf("%s does not contain a job line.", p))
		return false, diags
	case parsed.jobs > 1:
		diags.AddWarning("Several cron jobs in one file",
			fmt.Sprintf("%s contains %d job lines. sysutils_cron_job manages a single job; the next apply removes all but the first.", p, parsed.jobs))
	}
	job := parsed.job
	// Without a job line, the empty values plan an update that writes one.
	m.Schedule = types.StringValue(job.schedule)
	m.User = types.StringValue(job.user)
	m.Command = types.StringValue(job.command)

	if len(job.env) == 0 && m.Environment.IsNull() {
		// Keep an unset environment unset.
	} else {
		env, d := types.MapValueFrom(ctx, types.StringType, job.env)
		diags.Append(d...)
		m.Environment = env
	}
	switch {
	case job.hasComment && job.comment != "":
		m.Comment = types.StringValue(job.comment)
	case job.hasComment && !m.Comment.IsNull():
		// An empty comment line; "" in the configuration writes none, so
		// this is drift unless the configuration has "".
		m.Comment = types.StringValue(job.comment)
	case !job.hasComment && !m.Comment.IsNull() && m.Comment.ValueString() != "":
		m.Comment = types.StringNull()
	}

	m.ID = types.StringValue(name)
	m.Path = types.StringValue(p)
	m.Content = types.StringValue(strings.ToValidUTF8(string(data), "�"))
	m.Mode = types.StringValue(formatMode(snap.mode))
	m.Owner = types.StringValue(uidName(snap.uid))
	m.Group = types.StringValue(gidName(snap.gid))
	return false, diags
}

func (r *cronJobResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state cronJobModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	// State is not validated by the schema; never remove an arbitrary file.
	if err := validateCronName(name); err != nil {
		resp.Diagnostics.AddError("Invalid state", capitalize(err.Error())+".")
		return
	}
	p := r.cfg.jobPath(name)
	if err := removeCronFile(p); err != nil {
		resp.Diagnostics.AddError("Removing cron job", fmt.Sprintf("Removing %s: %s.", p, err))
	}
}

// ImportState imports a cron job by the name of its file in cron.d.
func (r *cronJobResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if err := validateCronName(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("Import ID must be the name of a file in the cron.d directory: %s.", capitalize(err.Error())))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
