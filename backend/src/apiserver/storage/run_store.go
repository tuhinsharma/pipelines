// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package storage

import (
	"database/sql"
	"fmt"

	sq "github.com/Masterminds/squirrel"
	workflowapi "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/golang/glog"

	api "github.com/kubeflow/pipelines/backend/api/go_client"
	"github.com/kubeflow/pipelines/backend/src/apiserver/common"
	"github.com/kubeflow/pipelines/backend/src/apiserver/list"
	"github.com/kubeflow/pipelines/backend/src/apiserver/metadata"
	"github.com/kubeflow/pipelines/backend/src/apiserver/model"
	"github.com/kubeflow/pipelines/backend/src/common/util"
	"k8s.io/apimachinery/pkg/util/json"
)

type RunStoreInterface interface {
	GetRun(runId string) (*model.RunDetail, error)

	ListRuns(filterContext *common.FilterContext, opts *list.Options) ([]*model.Run, int, string, error)

	// Create a run entry in the database
	CreateRun(run *model.RunDetail) (*model.RunDetail, error)

	// Update run table. Only condition and runtime manifest is allowed to be updated.
	UpdateRun(id string, condition string, workflowRuntimeManifest string) (err error)

	// Archive a run
	ArchiveRun(id string) error

	// Unarchive a run
	UnarchiveRun(id string) error

	// Delete a run entry from the database
	DeleteRun(id string) error

	// Update the run table or create one if the run doesn't exist
	CreateOrUpdateRun(run *model.RunDetail) error

	// Store a new metric entry to run_metrics table.
	ReportMetric(metric *model.RunMetric) (err error)

	// Terminate a run
	TerminateRun(runId string) error
}

type RunStore struct {
	db                     *DB
	resourceReferenceStore *ResourceReferenceStore
	time                   util.TimeInterface
	metadataStore          *metadata.Store
}

// Runs two SQL queries in a transaction to return a list of matching runs, as well as their
// total_size. The total_size does not reflect the page size, but it does reflect the number of runs
// matching the supplied filters and resource references.
func (s *RunStore) ListRuns(
	filterContext *common.FilterContext, opts *list.Options) ([]*model.Run, int, string, error) {
	errorF := func(err error) ([]*model.Run, int, string, error) {
		return nil, 0, "", util.NewInternalServerError(err, "Failed to list runs: %v", err)
	}

	rowsSql, rowsArgs, err := s.buildSelectRunsQuery(false, opts, filterContext)
	if err != nil {
		return errorF(err)
	}

	sizeSql, sizeArgs, err := s.buildSelectRunsQuery(true, opts, filterContext)
	if err != nil {
		return errorF(err)
	}

	// Use a transaction to make sure we're returning the total_size of the same rows queried
	tx, err := s.db.Begin()
	if err != nil {
		glog.Error("Failed to start transaction to list runs")
		return errorF(err)
	}

	rows, err := tx.Query(rowsSql, rowsArgs...)
	if err != nil {
		return errorF(err)
	}
	runDetails, err := s.scanRowsToRunDetails(rows)
	if err != nil {
		tx.Rollback()
		return errorF(err)
	}
	rows.Close()

	sizeRow, err := tx.Query(sizeSql, sizeArgs...)
	if err != nil {
		tx.Rollback()
		return errorF(err)
	}
	total_size, err := list.ScanRowToTotalSize(sizeRow)
	if err != nil {
		tx.Rollback()
		return errorF(err)
	}
	sizeRow.Close()

	err = tx.Commit()
	if err != nil {
		glog.Error("Failed to commit transaction to list runs")
		return errorF(err)
	}

	var runs []*model.Run
	for _, rd := range runDetails {
		r := rd.Run
		runs = append(runs, &r)
	}

	if len(runs) <= opts.PageSize {
		return runs, total_size, "", nil
	}

	npt, err := opts.NextPageToken(runs[opts.PageSize])
	return runs[:opts.PageSize], total_size, npt, err
}

func (s *RunStore) buildSelectRunsQuery(selectCount bool, opts *list.Options,
	filterContext *common.FilterContext) (string, []interface{}, error) {
	filteredSelectBuilder, err := list.FilterOnResourceReference("run_details", common.Run, selectCount, filterContext)
	if err != nil {
		return "", nil, util.NewInternalServerError(err, "Failed to list runs: %v", err)
	}

	sqlBuilder := opts.AddFilterToSelect(filteredSelectBuilder)
	if err != nil {
		return "", nil, util.NewInternalServerError(err, "Failed to list runs: %v", err)
	}

	// If we're not just counting, then also add select columns and perform a left join
	// to get resource reference information. Also add pagination.
	if !selectCount {
		sqlBuilder = s.addMetricsAndResourceReferences(sqlBuilder)
		sqlBuilder = opts.AddPaginationToSelect(sqlBuilder)
	}
	sql, args, err := sqlBuilder.ToSql()
	if err != nil {
		return "", nil, util.NewInternalServerError(err, "Failed to list runs: %v", err)
	}

	return sql, args, err
}

// GetRun Get the run manifest from Workflow CRD
func (s *RunStore) GetRun(runId string) (*model.RunDetail, error) {
	sql, args, err := s.addMetricsAndResourceReferences(sq.Select("*").From("run_details")).
		Where(sq.Eq{"UUID": runId}).
		Limit(1).
		ToSql()

	if err != nil {
		return nil, util.NewInternalServerError(err, "Failed to get run: %v", err.Error())
	}
	r, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, util.NewInternalServerError(err, "Failed to get run: %v", err.Error())
	}
	defer r.Close()
	runs, err := s.scanRowsToRunDetails(r)

	if err != nil || len(runs) > 1 {
		return nil, util.NewInternalServerError(err, "Failed to get run: %v", err.Error())
	}
	if len(runs) == 0 {
		return nil, util.NewResourceNotFoundError("Run", fmt.Sprint(runId))
	}
	if runs[0].WorkflowRuntimeManifest == "" {
		// This can only happen when workflow reporting is failed.
		return nil, util.NewResourceNotFoundError("Failed to get run: %s", runId)
	}
	return runs[0], nil
}

func (s *RunStore) addMetricsAndResourceReferences(filteredSelectBuilder sq.SelectBuilder) sq.SelectBuilder {
	metricConcatQuery := s.db.Concat([]string{`"["`, s.db.GroupConcat("m.Payload", ","), `"]"`}, "")
	subQ := sq.
		Select("rd.*", metricConcatQuery+" AS metrics").
		FromSelect(filteredSelectBuilder, "rd").
		LeftJoin("run_metrics AS m ON rd.UUID=m.RunUUID").
		GroupBy("rd.UUID")

	resourceRefConcatQuery := s.db.Concat([]string{`"["`, s.db.GroupConcat("r.Payload", ","), `"]"`}, "")
	return sq.
		Select("subq.*", resourceRefConcatQuery+" AS refs").
		FromSelect(subQ, "subq").
		// Append all the resource references for the run as a json column
		LeftJoin("(select * from resource_references where ResourceType='Run') AS r ON subq.UUID=r.ResourceUUID").
		GroupBy("subq.UUID")
}

func (s *RunStore) scanRowsToRunDetails(rows *sql.Rows) ([]*model.RunDetail, error) {
	var runs []*model.RunDetail
	for rows.Next() {
		var uuid, displayName, name, storageState, namespace, description, pipelineId, pipelineSpecManifest,
			workflowSpecManifest, parameters, conditions, pipelineRuntimeManifest, workflowRuntimeManifest string
		var createdAtInSec, scheduledAtInSec int64
		var metricsInString, resourceReferencesInString sql.NullString
		err := rows.Scan(
			&uuid,
			&displayName,
			&name,
			&storageState,
			&namespace,
			&description,
			&createdAtInSec,
			&scheduledAtInSec,
			&conditions,
			&pipelineId,
			&pipelineSpecManifest,
			&workflowSpecManifest,
			&parameters,
			&pipelineRuntimeManifest,
			&workflowRuntimeManifest,
			&metricsInString,
			&resourceReferencesInString,
		)
		if err != nil {
			glog.Errorf("Failed to scan row: %v", err)
			return runs, nil
		}
		metrics, err := parseMetrics(metricsInString)
		if err != nil {
			glog.Errorf("Failed to parse metrics (%v) from DB: %v", metricsInString, err)
			// Skip the error to allow user to get runs even when metrics data
			// are invalid.
			metrics = []*model.RunMetric{}
		}
		resourceReferences, err := parseResourceReferences(resourceReferencesInString)
		if err != nil {
			// throw internal exception if failed to parse the resource reference.
			return nil, util.NewInternalServerError(err, "Failed to parse resource reference.")
		}
		runs = append(runs, &model.RunDetail{Run: model.Run{
			UUID:               uuid,
			DisplayName:        displayName,
			Name:               name,
			StorageState:       storageState,
			Namespace:          namespace,
			Description:        description,
			CreatedAtInSec:     createdAtInSec,
			ScheduledAtInSec:   scheduledAtInSec,
			Conditions:         conditions,
			Metrics:            metrics,
			ResourceReferences: resourceReferences,
			PipelineSpec: model.PipelineSpec{
				PipelineId:           pipelineId,
				PipelineSpecManifest: pipelineRuntimeManifest,
				WorkflowSpecManifest: workflowSpecManifest,
				Parameters:           parameters,
			},
		},
			PipelineRuntime: model.PipelineRuntime{
				PipelineRuntimeManifest: pipelineRuntimeManifest,
				WorkflowRuntimeManifest: workflowRuntimeManifest}})
	}
	return runs, nil
}

func parseMetrics(metricsInString sql.NullString) ([]*model.RunMetric, error) {
	if !metricsInString.Valid {
		return nil, nil
	}
	var metrics []*model.RunMetric
	if err := json.Unmarshal([]byte(metricsInString.String), &metrics); err != nil {
		return nil, fmt.Errorf("failed unmarshal metrics '%s'. error: %v", metricsInString.String, err)
	}
	return metrics, nil
}

func parseResourceReferences(resourceRefString sql.NullString) ([]*model.ResourceReference, error) {
	if !resourceRefString.Valid {
		return nil, nil
	}
	var refs []*model.ResourceReference
	if err := json.Unmarshal([]byte(resourceRefString.String), &refs); err != nil {
		return nil, fmt.Errorf("failed unmarshal resource references '%s'. error: %v", resourceRefString.String, err)
	}
	return refs, nil
}

func (s *RunStore) CreateRun(r *model.RunDetail) (*model.RunDetail, error) {
	if r.StorageState == "" {
		r.StorageState = api.Run_STORAGESTATE_AVAILABLE.String()
	} else if r.StorageState != api.Run_STORAGESTATE_AVAILABLE.String() &&
		r.StorageState != api.Run_STORAGESTATE_ARCHIVED.String() {
		return nil, util.NewInvalidInputError("Invalid value for StorageState field: %q.", r.StorageState)
	}

	runSql, runArgs, err := sq.
		Insert("run_details").
		SetMap(sq.Eq{
			"UUID":                    r.UUID,
			"DisplayName":             r.DisplayName,
			"Name":                    r.Name,
			"StorageState":            r.StorageState,
			"Namespace":               r.Namespace,
			"Description":             r.Description,
			"CreatedAtInSec":          r.CreatedAtInSec,
			"ScheduledAtInSec":        r.ScheduledAtInSec,
			"Conditions":              r.Conditions,
			"WorkflowRuntimeManifest": r.WorkflowRuntimeManifest,
			"PipelineRuntimeManifest": r.PipelineRuntimeManifest,
			"PipelineId":              r.PipelineId,
			"PipelineSpecManifest":    r.PipelineSpecManifest,
			"WorkflowSpecManifest":    r.WorkflowSpecManifest,
			"Parameters":              r.Parameters,
		}).ToSql()
	if err != nil {
		return nil, util.NewInternalServerError(err, "Failed to create query to store run to run table: '%v/%v",
			r.Namespace, r.Name)
	}

	// Use a transaction to make sure both run and its resource references are stored.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, util.NewInternalServerError(err, "Failed to create a new transaction to create run.")
	}
	_, err = tx.Exec(runSql, runArgs...)
	if err != nil {
		tx.Rollback()
		return nil, util.NewInternalServerError(err, "Failed to store run %v to table", r.Name)
	}

	err = s.resourceReferenceStore.CreateResourceReferences(tx, r.ResourceReferences)
	if err != nil {
		tx.Rollback()
		return nil, util.NewInternalServerError(err, "Failed to store resource references to table for run %v ", r.Name)
	}
	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return nil, util.NewInternalServerError(err, "Failed to store run %v and its resource references to table", r.Name)
	}
	return r, nil
}

func (s *RunStore) UpdateRun(runID string, condition string, workflowRuntimeManifest string) (err error) {
	tx, err := s.db.DB.Begin()
	if err != nil {
		return util.NewInternalServerError(err, "transaction creation failed")
	}

	// Lock the row for update, so we ensure no other update of the same run
	// happens while we're parsing it for metadata. We rely on per-row updates
	// being synchronous, so metadata can be recorded at most once. Right now,
	// persistence agent will call UpdateRun all the time, even if there is nothing
	// new in the status of an Argo manifest. This means we need to keep track
	// manually here on what the previously updated state of the run is, to ensure
	// we do not add duplicate metadata. Hence the locking below.
	query := "SELECT WorkflowRuntimeManifest FROM run_details WHERE UUID = ?"
	query = s.db.SelectForUpdate(query)

	row := tx.QueryRow(query, runID)
	var storedManifest string
	if err := row.Scan(&storedManifest); err != nil {
		tx.Rollback()
		return util.NewInvalidInputError("Failed to update run %s. Row not found.", runID)
	}

	if err := s.metadataStore.RecordOutputArtifacts(runID, storedManifest, workflowRuntimeManifest); err != nil {
		// Metadata storage failed. Log the error here, but continue to allow the run
		// to be updated as per usual.
		glog.Errorf("Failed to record output artifacts: %+v", err)
	}

	sql, args, err := sq.
		Update("run_details").
		SetMap(sq.Eq{
			"Conditions":              condition,
			"WorkflowRuntimeManifest": workflowRuntimeManifest}).
		Where(sq.Eq{"UUID": runID}).
		ToSql()
	if err != nil {
		tx.Rollback()
		return util.NewInternalServerError(err,
			"Failed to create query to update run %s. error: '%v'", runID, err.Error())
	}
	result, err := tx.Exec(sql, args...)
	if err != nil {
		tx.Rollback()
		return util.NewInternalServerError(err,
			"Failed to update run %s. error: '%v'", runID, err.Error())
	}
	if r, _ := result.RowsAffected(); r != 1 {
		tx.Rollback()
		return util.NewInvalidInputError("Failed to update run %s. Row not found.", runID)
	}
	if err := tx.Commit(); err != nil {
		return util.NewInternalServerError(err, "failed to commit transaction")
	}
	return nil
}

func (s *RunStore) CreateOrUpdateRun(runDetail *model.RunDetail) error {
	_, createError := s.CreateRun(runDetail)
	if createError == nil {
		return nil
	}

	updateError := s.UpdateRun(runDetail.UUID, runDetail.Conditions, runDetail.WorkflowRuntimeManifest)
	if updateError != nil {
		return util.Wrap(updateError, fmt.Sprintf(
			"Error while creating or updating run for workflow: '%v/%v'. Create error: '%v'. Update error: '%v'",
			runDetail.Namespace, runDetail.Name, createError.Error(), updateError.Error()))
	}
	return nil
}

func (s *RunStore) ArchiveRun(runId string) error {
	sql, args, err := sq.
		Update("run_details").
		SetMap(sq.Eq{
			"StorageState": api.Run_STORAGESTATE_ARCHIVED.String(),
		}).
		Where(sq.Eq{"UUID": runId}).
		ToSql()

	if err != nil {
		return util.NewInternalServerError(err,
			"Failed to create query to archive run %s. error: '%v'", runId, err.Error())
	}

	_, err = s.db.Exec(sql, args...)
	if err != nil {
		return util.NewInternalServerError(err,
			"Failed to archive run %s. error: '%v'", runId, err.Error())
	}

	return nil
}

func (s *RunStore) UnarchiveRun(runId string) error {
	sql, args, err := sq.
		Update("run_details").
		SetMap(sq.Eq{
			"StorageState": api.Run_STORAGESTATE_AVAILABLE.String(),
		}).
		Where(sq.Eq{"UUID": runId}).
		ToSql()

	if err != nil {
		return util.NewInternalServerError(err,
			"Failed to create query to unarchive run %s. error: '%v'", runId, err.Error())
	}

	_, err = s.db.Exec(sql, args...)
	if err != nil {
		return util.NewInternalServerError(err,
			"Failed to unarchive run %s. error: '%v'", runId, err.Error())
	}

	return nil
}

func (s *RunStore) DeleteRun(id string) error {
	runSql, runArgs, err := sq.Delete("run_details").Where(sq.Eq{"UUID": id}).ToSql()
	if err != nil {
		return util.NewInternalServerError(err,
			"Failed to create query to delete run: %s", id)
	}
	// Use a transaction to make sure both run and its resource references are stored.
	tx, err := s.db.Begin()
	if err != nil {
		return util.NewInternalServerError(err, "Failed to create a new transaction to delete run.")
	}
	_, err = tx.Exec(runSql, runArgs...)
	if err != nil {
		tx.Rollback()
		return util.NewInternalServerError(err, "Failed to delete run %s from table", id)
	}
	err = s.resourceReferenceStore.DeleteResourceReferences(tx, id, common.Run)
	if err != nil {
		tx.Rollback()
		return util.NewInternalServerError(err, "Failed to delete resource references from table for run %v ", id)
	}
	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return util.NewInternalServerError(err, "Failed to delete run %v and its resource references from table", id)
	}
	return nil
}

// ReportMetric inserts a new metric to run_metrics table. Conflicting metrics
// are ignored.
func (s *RunStore) ReportMetric(metric *model.RunMetric) (err error) {
	payloadBytes, err := json.Marshal(metric)
	if err != nil {
		return util.NewInternalServerError(err,
			"failed to marshal metric to json: %+v", metric)
	}
	sql, args, err := sq.
		Insert("run_metrics").
		SetMap(sq.Eq{
			"RunUUID":     metric.RunUUID,
			"NodeID":      metric.NodeID,
			"Name":        metric.Name,
			"NumberValue": metric.NumberValue,
			"Format":      metric.Format,
			"Payload":     string(payloadBytes)}).ToSql()
	if err != nil {
		return util.NewInternalServerError(err,
			"failed to create query for inserting metric: %+v", metric)
	}
	_, err = s.db.Exec(sql, args...)
	if err != nil {
		if s.db.IsDuplicateError(err) {
			return util.NewAlreadyExistError(
				"same metric has been reported before: %s/%s", metric.NodeID, metric.Name)
		}
		return util.NewInternalServerError(err, "failed to insert metric: %v", metric)
	}
	return nil
}

func (s *RunStore) toListableModels(runs []model.RunDetail) []model.ListableDataModel {
	models := make([]model.ListableDataModel, len(runs))
	for i := range models {
		models[i] = runs[i].Run
	}
	return models
}

func (s *RunStore) toRunMetadatas(models []model.ListableDataModel) []model.Run {
	runMetadatas := make([]model.Run, len(models))
	for i := range models {
		runMetadatas[i] = models[i].(model.Run)
	}
	return runMetadatas
}

// NewRunStore creates a new RunStore. If metadataStore is non-nil, it will be
// used to record artifact metadata.
func NewRunStore(db *DB, time util.TimeInterface, metadataStore *metadata.Store) *RunStore {
	return &RunStore{
		db:                     db,
		resourceReferenceStore: NewResourceReferenceStore(db),
		time:                   time,
		metadataStore:          metadataStore,
	}
}

func (s *RunStore) TerminateRun(runId string) error {
	result, err := s.db.Exec(`
		UPDATE run_details
		SET Conditions = "Terminating"
		WHERE UUID = ? AND (Conditions = ? OR Conditions = ? OR Conditions = ?)`,
		runId, string(workflowapi.NodeRunning), string(workflowapi.NodePending), "")

	if err != nil {
		return util.NewInternalServerError(err,
			"Failed to terminate run %s. error: '%v'", runId, err.Error())
	}

	if r, _ := result.RowsAffected(); r != 1 {
		return util.NewInvalidInputError("Failed to terminate run %s. Row not found.", runId)
	}

	return nil
}
