/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"context"
	"fmt"
	"strconv"

	"knative.dev/pkg/kmeta"

	"github.com/tektoncd/pipeline/pkg/apis/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	resourcev1alpha1 "github.com/tektoncd/pipeline/pkg/apis/resource/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/list"
	"github.com/tektoncd/pipeline/pkg/reconciler/taskrun/resources"
	"k8s.io/apimachinery/pkg/api/errors"
	"knative.dev/pkg/apis"
)

const (
	// ReasonConditionCheckFailed indicates that the reason for the failure status is that the
	// condition check associated to the pipeline task evaluated to false
	ReasonConditionCheckFailed = "ConditionCheckFailed"
)

// SkippingReason explains why a task was skipped
type SkippingReason string

const (
	// WhenExpressionsSkip means the task was skipped due to at least one of its when expressions evaluating to false
	WhenExpressionsSkip SkippingReason = "WhenExpressionsSkip"
	// ConditionsSkip means the task was skipped due to at least one of its conditions failing
	ConditionsSkip SkippingReason = "ConditionsSkip"
	// ParentTasksSkip means the task was skipped because its parent was skipped
	ParentTasksSkip SkippingReason = "ParentTasksSkip"
	// IsStoppingSkip means the task was skipped because the pipeline run is stopping
	IsStoppingSkip SkippingReason = "IsStoppingSkip"
	// IsGracefullyCancelledSkip means the task was skipped because the pipeline run has been gracefully cancelled
	IsGracefullyCancelledSkip SkippingReason = "IsGracefullyCancelledSkip"
	// IsGracefullyStoppedSkip means the task was skipped because the pipeline run has been gracefully stopped
	IsGracefullyStoppedSkip SkippingReason = "IsGracefullyStoppedSkip"
	// MissingResultsSkip means the task was skipped because it's missing necessary results
	MissingResultsSkip SkippingReason = "MissingResultsSkip"
	// None means the task was not skipped
	None SkippingReason = "None"
)

// TaskSkipStatus stores whether a task was skipped and why
type TaskSkipStatus struct {
	IsSkipped      bool
	SkippingReason SkippingReason
}

// TaskNotFoundError indicates that the resolution failed because a referenced Task couldn't be retrieved
type TaskNotFoundError struct {
	Name string
	Msg  string
}

func (e *TaskNotFoundError) Error() string {
	return fmt.Sprintf("Couldn't retrieve Task %q: %s", e.Name, e.Msg)
}

// ConditionNotFoundError is used to track failures to the
type ConditionNotFoundError struct {
	Name string
	Msg  string
}

func (e *ConditionNotFoundError) Error() string {
	return fmt.Sprintf("Couldn't retrieve Condition %q: %s", e.Name, e.Msg)
}

// ResolvedPipelineRunTask contains a Task and its associated TaskRun, if it
// exists. TaskRun can be nil to represent there being no TaskRun.
type ResolvedPipelineRunTask struct {
	TaskRunName string
	TaskRun     *v1beta1.TaskRun
	// If the PipelineTask is a Custom Task, RunName and Run will be set.
	CustomTask            bool
	RunName               string
	Run                   *v1alpha1.Run
	PipelineTask          *v1beta1.PipelineTask
	ResolvedTaskResources *resources.ResolvedTaskResources
	// ConditionChecks ~~TaskRuns but for evaling conditions
	ResolvedConditionChecks TaskConditionCheckState // Could also be a TaskRun or maybe just a Pod?
}

// IsDone returns true only if the task is skipped, succeeded or failed
func (t ResolvedPipelineRunTask) IsDone(facts *PipelineRunFacts) bool {
	return t.Skip(facts).IsSkipped || t.IsSuccessful() || t.IsFailure()
}

// IsRunning returns true only if the task is neither succeeded, cancelled nor failed
func (t ResolvedPipelineRunTask) IsRunning() bool {
	if t.IsCustomTask() {
		if t.Run == nil {
			return false
		}
	} else {
		if t.TaskRun == nil {
			return false
		}
	}
	return !t.IsSuccessful() && !t.IsFailure() && !t.IsCancelled()
}

// IsCustomTask returns true if the PipelineTask references a Custom Task.
func (t ResolvedPipelineRunTask) IsCustomTask() bool {
	return t.CustomTask
}

// IsSuccessful returns true only if the run has completed successfully
func (t ResolvedPipelineRunTask) IsSuccessful() bool {
	if t.IsCustomTask() {
		return t.Run != nil && t.Run.IsSuccessful()
	}
	return t.TaskRun != nil && t.TaskRun.IsSuccessful()
}

// IsFailure returns true only if the run has failed and will not be retried.
func (t ResolvedPipelineRunTask) IsFailure() bool {
	if t.IsCancelled() {
		return true
	}
	if t.IsSuccessful() {
		return false
	}
	var c *apis.Condition
	var isDone bool
	if t.IsCustomTask() {
		if t.Run == nil {
			return false
		}
		c = t.Run.Status.GetCondition(apis.ConditionSucceeded)
		isDone = t.Run.IsDone()
	} else {
		if t.TaskRun == nil {
			return false
		}
		c = t.TaskRun.Status.GetCondition(apis.ConditionSucceeded)
		isDone = t.TaskRun.IsDone()
	}
	return isDone && c.IsFalse() && !t.HasRemainingRetries()
}

// HasRemainingRetries returns true only when the number of retries already attempted
// is less than the number of retries allowed.
func (t ResolvedPipelineRunTask) HasRemainingRetries() bool {
	var retriesDone int
	if t.IsCustomTask() {
		if t.Run == nil {
			return true
		}
		retriesDone = len(t.Run.Status.RetriesStatus)
	} else {
		if t.TaskRun == nil {
			return true
		}
		retriesDone = len(t.TaskRun.Status.RetriesStatus)
	}
	return retriesDone < t.PipelineTask.Retries
}

// IsCancelled returns true only if the run is cancelled
func (t ResolvedPipelineRunTask) IsCancelled() bool {
	if t.IsCustomTask() {
		if t.Run == nil {
			return false
		}
		c := t.Run.Status.GetCondition(apis.ConditionSucceeded)
		return c != nil && c.IsFalse() && c.Reason == v1alpha1.RunReasonCancelled
	}
	if t.TaskRun == nil {
		return false
	}
	c := t.TaskRun.Status.GetCondition(apis.ConditionSucceeded)
	return c != nil && c.IsFalse() && c.Reason == v1beta1.TaskRunReasonCancelled.String()
}

// IsStarted returns true only if the PipelineRunTask itself has a TaskRun or
// Run associated that has a Succeeded-type condition.
func (t ResolvedPipelineRunTask) IsStarted() bool {
	if t.IsCustomTask() {
		return t.Run != nil && t.Run.Status.GetCondition(apis.ConditionSucceeded) != nil

	}
	return t.TaskRun != nil && t.TaskRun.Status.GetCondition(apis.ConditionSucceeded) != nil
}

// IsConditionStatusFalse returns true when a task has succeeded condition with status set to false
// it includes task failed after retries are exhausted, cancelled tasks, and time outs
func (t ResolvedPipelineRunTask) IsConditionStatusFalse() bool {
	if t.IsStarted() {
		if t.IsCustomTask() {
			return t.Run.Status.GetCondition(apis.ConditionSucceeded).IsFalse()
		}
		return t.TaskRun.Status.GetCondition(apis.ConditionSucceeded).IsFalse()
	}
	return false
}

func (t *ResolvedPipelineRunTask) checkParentsDone(facts *PipelineRunFacts) bool {
	if facts.isFinalTask(t.PipelineTask.Name) {
		return true
	}
	stateMap := facts.State.ToMap()
	node := facts.TasksGraph.Nodes[t.PipelineTask.Name]
	for _, p := range node.Prev {
		if !stateMap[p.Task.HashKey()].IsDone(facts) {
			return false
		}
	}
	return true
}

func (t *ResolvedPipelineRunTask) skip(facts *PipelineRunFacts) TaskSkipStatus {
	var skippingReason SkippingReason

	switch {
	case facts.isFinalTask(t.PipelineTask.Name) || t.IsStarted():
		skippingReason = None
	case facts.IsStopping():
		skippingReason = IsStoppingSkip
	case facts.IsGracefullyCancelled():
		skippingReason = IsGracefullyCancelledSkip
	case facts.IsGracefullyStopped():
		skippingReason = IsGracefullyStoppedSkip
	case t.skipBecauseParentTaskWasSkipped(facts):
		skippingReason = ParentTasksSkip
	case t.skipBecauseConditionsFailed():
		skippingReason = ConditionsSkip
	case t.skipBecauseResultReferencesAreMissing(facts):
		skippingReason = MissingResultsSkip
	case t.skipBecauseWhenExpressionsEvaluatedToFalse(facts):
		skippingReason = WhenExpressionsSkip
	default:
		skippingReason = None
	}

	return TaskSkipStatus{
		IsSkipped:      skippingReason != None,
		SkippingReason: skippingReason,
	}
}

// Skip returns true if a PipelineTask will not be run because
// (1) its When Expressions evaluated to false
// (2) its Condition Checks failed
// (3) its parent task was skipped
// (4) Pipeline is in stopping state (one of the PipelineTasks failed)
// (5) Pipeline is gracefully cancelled or stopped
// Note that this means Skip returns false if a conditionCheck is in progress
func (t *ResolvedPipelineRunTask) Skip(facts *PipelineRunFacts) TaskSkipStatus {
	if facts.SkipCache == nil {
		facts.SkipCache = make(map[string]TaskSkipStatus)
	}
	if _, cached := facts.SkipCache[t.PipelineTask.Name]; !cached {
		facts.SkipCache[t.PipelineTask.Name] = t.skip(facts)
	}
	return facts.SkipCache[t.PipelineTask.Name]
}

// skipBecauseConditionsFailed checks that the task has Conditions which have completed evaluating
// it returns true if any of the Conditions fails
func (t *ResolvedPipelineRunTask) skipBecauseConditionsFailed() bool {
	if len(t.ResolvedConditionChecks) > 0 {
		if t.ResolvedConditionChecks.IsDone() && !t.ResolvedConditionChecks.IsSuccess() {
			return true
		}
	}
	return false
}

// skipBecauseWhenExpressionsEvaluatedToFalse confirms that the when expressions have completed evaluating, and
// it returns true if any of the when expressions evaluate to false
func (t *ResolvedPipelineRunTask) skipBecauseWhenExpressionsEvaluatedToFalse(facts *PipelineRunFacts) bool {
	if t.checkParentsDone(facts) {
		if !t.PipelineTask.WhenExpressions.AllowsExecution() {
			return true
		}
	}
	return false
}

// skipBecauseParentTaskWasSkipped loops through the parent tasks and checks if the parent task skipped:
//    if yes, is it because of when expressions?
//        if yes, it ignores this parent skip and continue evaluating other parent tasks
//        if no, it returns true to skip the current task because this parent task was skipped
//    if no, it continues checking the other parent tasks
func (t *ResolvedPipelineRunTask) skipBecauseParentTaskWasSkipped(facts *PipelineRunFacts) bool {
	stateMap := facts.State.ToMap()
	node := facts.TasksGraph.Nodes[t.PipelineTask.Name]
	for _, p := range node.Prev {
		parentTask := stateMap[p.Task.HashKey()]
		if parentSkipStatus := parentTask.Skip(facts); parentSkipStatus.IsSkipped {
			// if the parent task was skipped due to its `when` expressions,
			// then we should ignore that and continue evaluating if we should skip because of other parent tasks
			if parentSkipStatus.SkippingReason == WhenExpressionsSkip {
				continue
			}
			return true
		}
	}
	return false
}

// skipBecauseResultReferencesAreMissing checks if the task references results that cannot be resolved, which is a
// reason for skipping the task, and applies result references if found
func (t *ResolvedPipelineRunTask) skipBecauseResultReferencesAreMissing(facts *PipelineRunFacts) bool {
	if t.checkParentsDone(facts) && t.hasResultReferences() {
		resolvedResultRefs, pt, err := ResolveResultRefs(facts.State, PipelineRunState{t})
		rprt := facts.State.ToMap()[pt]
		if err != nil && (t.IsFinalTask(facts) || rprt.Skip(facts).SkippingReason == WhenExpressionsSkip) {
			return true
		}
		ApplyTaskResults(PipelineRunState{t}, resolvedResultRefs)
		facts.ResetSkippedCache()
	}
	return false
}

// IsFinalTask returns true if a task is a finally task
func (t *ResolvedPipelineRunTask) IsFinalTask(facts *PipelineRunFacts) bool {
	return facts.isFinalTask(t.PipelineTask.Name)
}

// IsFinallySkipped returns true if a finally task is not executed and skipped due to task result validation failure
func (t *ResolvedPipelineRunTask) IsFinallySkipped(facts *PipelineRunFacts) TaskSkipStatus {
	var skippingReason SkippingReason

	switch {
	case t.IsStarted():
		skippingReason = None
	case facts.checkDAGTasksDone() && facts.isFinalTask(t.PipelineTask.Name):
		switch {
		case t.skipBecauseResultReferencesAreMissing(facts):
			skippingReason = MissingResultsSkip
		case t.skipBecauseWhenExpressionsEvaluatedToFalse(facts):
			skippingReason = WhenExpressionsSkip
		default:
			skippingReason = None
		}
	default:
		skippingReason = None
	}

	return TaskSkipStatus{
		IsSkipped:      skippingReason != None,
		SkippingReason: skippingReason,
	}

}

// GetRun is a function that will retrieve a Run by name.
type GetRun func(name string) (*v1alpha1.Run, error)

// GetResourcesFromBindings will retrieve all Resources bound in PipelineRun pr and return a map
// from the declared name of the PipelineResource (which is how the PipelineResource will
// be referred to in the PipelineRun) to the PipelineResource, obtained via getResource.
func GetResourcesFromBindings(pr *v1beta1.PipelineRun, getResource resources.GetResource) (map[string]*resourcev1alpha1.PipelineResource, error) {
	rs := map[string]*resourcev1alpha1.PipelineResource{}
	for _, resource := range pr.Spec.Resources {
		r, err := resources.GetResourceFromBinding(resource, getResource)
		if err != nil {
			return rs, err
		}
		rs[resource.Name] = r
	}
	return rs, nil
}

// ValidateResourceBindings validate that the PipelineResources declared in Pipeline p are bound in PipelineRun.
func ValidateResourceBindings(p *v1beta1.PipelineSpec, pr *v1beta1.PipelineRun) error {
	required := make([]string, 0, len(p.Resources))
	optional := make([]string, 0, len(p.Resources))
	for _, resource := range p.Resources {
		if resource.Optional {
			// create a list of optional resources
			optional = append(optional, resource.Name)
		} else {
			// create a list of required resources
			required = append(required, resource.Name)
		}
	}
	provided := make([]string, 0, len(pr.Spec.Resources))
	for _, resource := range pr.Spec.Resources {
		provided = append(provided, resource.Name)
	}
	// verify that the list of required resources exists in the provided resources
	missing := list.DiffLeft(required, provided)
	if len(missing) > 0 {
		return fmt.Errorf("Pipeline's declared required resources are missing from the PipelineRun: %s", missing)
	}
	// verify that the list of provided resources does not have any extra resources (outside of required and optional resources combined)
	extra := list.DiffLeft(provided, append(required, optional...))
	if len(extra) > 0 {
		return fmt.Errorf("PipelineRun's declared resources didn't match usage in Pipeline: %s", extra)
	}
	return nil
}

// ValidateWorkspaceBindings validates that the Workspaces expected by a Pipeline are provided by a PipelineRun.
func ValidateWorkspaceBindings(p *v1beta1.PipelineSpec, pr *v1beta1.PipelineRun) error {
	pipelineRunWorkspaces := make(map[string]v1beta1.WorkspaceBinding)
	for _, binding := range pr.Spec.Workspaces {
		pipelineRunWorkspaces[binding.Name] = binding
	}

	for _, ws := range p.Workspaces {
		if ws.Optional {
			continue
		}
		if _, ok := pipelineRunWorkspaces[ws.Name]; !ok {
			return fmt.Errorf("pipeline requires workspace with name %q be provided by pipelinerun", ws.Name)
		}
	}
	return nil
}

// ValidateTaskRunSpecs that the TaskRunSpecs defined by a PipelineRun are correct.
func ValidateTaskRunSpecs(p *v1beta1.PipelineSpec, pr *v1beta1.PipelineRun) error {
	pipelineTasks := make(map[string]string)
	for _, task := range p.Tasks {
		pipelineTasks[task.Name] = task.Name
	}

	for _, task := range p.Finally {
		pipelineTasks[task.Name] = task.Name
	}

	for _, taskrunSpec := range pr.Spec.TaskRunSpecs {
		if _, ok := pipelineTasks[taskrunSpec.PipelineTaskName]; !ok {
			return fmt.Errorf("PipelineRun's taskrunSpecs defined wrong taskName: %q, does not exist in Pipeline", taskrunSpec.PipelineTaskName)
		}
	}
	return nil
}

// ValidateServiceaccountMapping validates that the ServiceAccountNames defined by a PipelineRun are not correct.
func ValidateServiceaccountMapping(p *v1beta1.PipelineSpec, pr *v1beta1.PipelineRun) error {
	pipelineTasks := make(map[string]string)
	for _, task := range p.Tasks {
		pipelineTasks[task.Name] = task.Name
	}

	for _, task := range p.Finally {
		pipelineTasks[task.Name] = task.Name
	}

	for _, name := range pr.Spec.ServiceAccountNames {
		if _, ok := pipelineTasks[name.TaskName]; !ok {
			return fmt.Errorf("PipelineRun's ServiceAccountNames defined wrong taskName: %q, does not exist in Pipeline", name.TaskName)
		}
	}
	return nil
}

func isCustomTask(ctx context.Context, rprt ResolvedPipelineRunTask) bool {
	invalidSpec := rprt.PipelineTask.TaskRef != nil && rprt.PipelineTask.TaskSpec != nil
	isTaskRefCustomTask := rprt.PipelineTask.TaskRef != nil && rprt.PipelineTask.TaskRef.APIVersion != "" &&
		rprt.PipelineTask.TaskRef.Kind != ""
	isTaskSpecCustomTask := rprt.PipelineTask.TaskSpec != nil && rprt.PipelineTask.TaskSpec.APIVersion != "" &&
		rprt.PipelineTask.TaskSpec.Kind != ""
	cfg := config.FromContextOrDefaults(ctx)
	return cfg.FeatureFlags.EnableCustomTasks && !invalidSpec && (isTaskRefCustomTask || isTaskSpecCustomTask)
}

// ResolvePipelineRunTask retrieves a single Task's instance using the getTask to fetch
// the spec. If it is unable to retrieve an instance of a referenced Task, it  will return
// an error, otherwise it returns a list of all of the Tasks retrieved.  It will retrieve
// the Resources needed for the TaskRun using the mapping of providedResources.
func ResolvePipelineRunTask(
	ctx context.Context,
	pipelineRun v1beta1.PipelineRun,
	getTask resources.GetTask,
	getTaskRun resources.GetTaskRun,
	getRun GetRun,
	getCondition GetCondition,
	task v1beta1.PipelineTask,
	providedResources map[string]*resourcev1alpha1.PipelineResource,
) (*ResolvedPipelineRunTask, error) {

	rprt := ResolvedPipelineRunTask{
		PipelineTask: &task,
	}
	rprt.CustomTask = isCustomTask(ctx, rprt)
	if rprt.IsCustomTask() {
		rprt.RunName = getRunName(pipelineRun.Status.Runs, pipelineRun.Status.ChildReferences, task.Name, pipelineRun.Name)
		run, err := getRun(rprt.RunName)
		if err != nil && !errors.IsNotFound(err) {
			return nil, fmt.Errorf("error retrieving Run %s: %w", rprt.RunName, err)
		}
		rprt.Run = run
	} else {
		rprt.TaskRunName = GetTaskRunName(pipelineRun.Status.TaskRuns, pipelineRun.Status.ChildReferences, task.Name, pipelineRun.Name)

		// Find the Task that this PipelineTask is using
		var (
			t        v1beta1.TaskObject
			err      error
			spec     v1beta1.TaskSpec
			taskName string
			kind     v1beta1.TaskKind
		)

		taskRun, err := getTaskRun(rprt.TaskRunName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("error retrieving TaskRun %s: %w", rprt.TaskRunName, err)
			}
		}
		if taskRun != nil {
			rprt.TaskRun = taskRun
		}

		if task.TaskRef != nil {
			// If the TaskRun has already a store TaskSpec in its status, use it as source of truth
			if taskRun != nil && taskRun.Status.TaskSpec != nil {
				spec = *taskRun.Status.TaskSpec
				taskName = task.TaskRef.Name
			} else {
				t, err = getTask(ctx, task.TaskRef.Name)
				if err != nil {
					return nil, &TaskNotFoundError{
						Name: task.TaskRef.Name,
						Msg:  err.Error(),
					}
				}
				spec = t.TaskSpec()
				taskName = t.TaskMetadata().Name
			}
			kind = task.TaskRef.Kind
		} else {
			spec = task.TaskSpec.TaskSpec
		}
		spec.SetDefaults(ctx)
		rtr, err := resolvePipelineTaskResources(task, &spec, taskName, kind, providedResources)
		if err != nil {
			return nil, fmt.Errorf("couldn't match referenced resources with declared resources: %w", err)
		}

		rprt.ResolvedTaskResources = rtr

		// Get all conditions that this pipelineTask will be using, if any
		if len(task.Conditions) > 0 {
			rcc, err := resolveConditionChecks(&task, pipelineRun.Status.TaskRuns, pipelineRun.Status.ChildReferences, rprt.TaskRunName, getTaskRun, getCondition, providedResources)
			if err != nil {
				return nil, err
			}
			rprt.ResolvedConditionChecks = rcc
		}
	}
	return &rprt, nil
}

// getConditionCheckName should return a unique name for a `ConditionCheck` if one has not already been defined, and the existing one otherwise.
func getConditionCheckName(taskRunStatus map[string]*v1beta1.PipelineRunTaskRunStatus, childRefs []v1beta1.ChildStatusReference, trName, conditionRegisterName string) string {
	for _, cr := range childRefs {
		if cr.Name == trName {
			for _, cc := range cr.ConditionChecks {
				if cc.ConditionName == conditionRegisterName {
					return cc.ConditionCheckName
				}
			}
		}
	}
	trStatus, ok := taskRunStatus[trName]
	if ok && trStatus.ConditionChecks != nil {
		for k, v := range trStatus.ConditionChecks {
			// TODO(1022): Should  we allow multiple conditions of the same type?
			if conditionRegisterName == v.ConditionName {
				return k
			}
		}
	}
	return kmeta.ChildName(trName, fmt.Sprintf("-%s", conditionRegisterName))
}

// GetTaskRunName should return a unique name for a `TaskRun` if one has not already been defined, and the existing one otherwise.
func GetTaskRunName(taskRunsStatus map[string]*v1beta1.PipelineRunTaskRunStatus, childRefs []v1beta1.ChildStatusReference, ptName, prName string) string {
	for _, cr := range childRefs {
		if cr.Kind == "TaskRun" && cr.PipelineTaskName == ptName {
			return cr.Name
		}
	}

	for k, v := range taskRunsStatus {
		if v.PipelineTaskName == ptName {
			return k
		}
	}

	return kmeta.ChildName(prName, fmt.Sprintf("-%s", ptName))
}

// getRunName should return a unique name for a `Run` if one has not already
// been defined, and the existing one otherwise.
func getRunName(runsStatus map[string]*v1beta1.PipelineRunRunStatus, childRefs []v1beta1.ChildStatusReference, ptName, prName string) string {
	for _, cr := range childRefs {
		if cr.Kind == "Run" && cr.PipelineTaskName == ptName {
			return cr.Name
		}
	}

	for k, v := range runsStatus {
		if v.PipelineTaskName == ptName {
			return k
		}
	}

	return kmeta.ChildName(prName, fmt.Sprintf("-%s", ptName))
}

func resolveConditionChecks(pt *v1beta1.PipelineTask, taskRunStatus map[string]*v1beta1.PipelineRunTaskRunStatus, childRefs []v1beta1.ChildStatusReference, taskRunName string, getTaskRun resources.GetTaskRun, getCondition GetCondition, providedResources map[string]*resourcev1alpha1.PipelineResource) ([]*ResolvedConditionCheck, error) {
	rccs := []*ResolvedConditionCheck{}
	for i := range pt.Conditions {
		ptc := pt.Conditions[i]
		cName := ptc.ConditionRef
		crName := fmt.Sprintf("%s-%s", cName, strconv.Itoa(i))
		c, err := getCondition(cName)
		if err != nil {
			return nil, &ConditionNotFoundError{
				Name: cName,
				Msg:  err.Error(),
			}
		}
		conditionCheckName := getConditionCheckName(taskRunStatus, childRefs, taskRunName, crName)
		// TODO(#3133): Also handle Custom Task Runs (getRun here)
		cctr, err := getTaskRun(conditionCheckName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("error retrieving ConditionCheck %s for taskRun name %s : %w", conditionCheckName, taskRunName, err)
			}
		}
		conditionResources := map[string]*resourcev1alpha1.PipelineResource{}
		for _, declared := range ptc.Resources {
			if r, ok := providedResources[declared.Resource]; ok {
				conditionResources[declared.Name] = r
			} else {
				for _, resource := range c.Spec.Resources {
					if declared.Name == resource.Name && !resource.Optional {
						return nil, fmt.Errorf("resources %s missing for condition %s in pipeline task %s", declared.Resource, cName, pt.Name)
					}
				}
			}
		}

		rcc := ResolvedConditionCheck{
			ConditionRegisterName: crName,
			Condition:             c,
			ConditionCheckName:    conditionCheckName,
			ConditionCheck:        v1beta1.NewConditionCheck(cctr),
			PipelineTaskCondition: &ptc,
			ResolvedResources:     conditionResources,
		}

		rccs = append(rccs, &rcc)
	}
	return rccs, nil
}

// resolvePipelineTaskResources matches PipelineResources referenced by pt inputs and outputs with the
// providedResources and returns an instance of ResolvedTaskResources.
func resolvePipelineTaskResources(pt v1beta1.PipelineTask, ts *v1beta1.TaskSpec, taskName string, kind v1beta1.TaskKind, providedResources map[string]*resourcev1alpha1.PipelineResource) (*resources.ResolvedTaskResources, error) {
	rtr := resources.ResolvedTaskResources{
		TaskName: taskName,
		TaskSpec: ts,
		Kind:     kind,
		Inputs:   map[string]*resourcev1alpha1.PipelineResource{},
		Outputs:  map[string]*resourcev1alpha1.PipelineResource{},
	}
	if pt.Resources != nil {
		for _, taskInput := range pt.Resources.Inputs {
			if resource, ok := providedResources[taskInput.Resource]; ok {
				rtr.Inputs[taskInput.Name] = resource
			} else {
				if ts.Resources == nil || ts.Resources.Inputs == nil {
					return nil, fmt.Errorf("pipelineTask tried to use input resource %s not present in declared resources", taskInput.Resource)
				}
				for _, r := range ts.Resources.Inputs {
					if r.Name == taskInput.Name && !r.Optional {
						return nil, fmt.Errorf("pipelineTask tried to use input resource %s not present in declared resources", taskInput.Resource)
					}
				}
			}
		}
		for _, taskOutput := range pt.Resources.Outputs {
			if resource, ok := providedResources[taskOutput.Resource]; ok {
				rtr.Outputs[taskOutput.Name] = resource
			} else {
				if ts.Resources == nil || ts.Resources.Outputs == nil {
					return nil, fmt.Errorf("pipelineTask tried to use output resource %s not present in declared resources", taskOutput.Resource)
				}
				for _, r := range ts.Resources.Outputs {
					if r.Name == taskOutput.Name && !r.Optional {
						return nil, fmt.Errorf("pipelineTask tried to use output resource %s not present in declared resources", taskOutput.Resource)
					}
				}
			}
		}
	}
	return &rtr, nil
}

func (t *ResolvedPipelineRunTask) hasResultReferences() bool {
	for _, param := range t.PipelineTask.Params {
		if ps, ok := v1beta1.GetVarSubstitutionExpressionsForParam(param); ok {
			if v1beta1.LooksLikeContainsResultRefs(ps) {
				return true
			}
		}
	}
	for _, we := range t.PipelineTask.WhenExpressions {
		if ps, ok := we.GetVarSubstitutionExpressions(); ok {
			if v1beta1.LooksLikeContainsResultRefs(ps) {
				return true
			}
		}
	}
	return false
}
