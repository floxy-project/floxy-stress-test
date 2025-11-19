package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/rom8726/chaoskit"
)

// QueueEmptyValidator checks that the workflow_queue table is empty
type QueueEmptyValidator struct{}

func NewQueueEmptyValidator() *QueueEmptyValidator {
	return &QueueEmptyValidator{}
}

func (v *QueueEmptyValidator) Name() string {
	return "queue-empty"
}

func (v *QueueEmptyValidator) Severity() chaoskit.ValidationSeverity {
	return chaoskit.SeverityCritical
}

func (v *QueueEmptyValidator) Validate(ctx context.Context, target chaoskit.Target) error {
	floxyTarget, ok := target.(*FloxyStressTarget)
	if !ok {
		return fmt.Errorf("target is not FloxyStressTarget")
	}

	var count int
	err := floxyTarget.pool.QueryRow(ctx, "SELECT COUNT(*) FROM workflows.workflow_queue").Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to query workflow_queue: %w", err)
	}

	if count > 0 {
		return fmt.Errorf("workflow_queue is not empty: found %d entries", count)
	}

	return nil
}

// CompletedInstancesValidator checks that for completed instances, all steps are also completed
type CompletedInstancesValidator struct{}

func NewCompletedInstancesValidator() *CompletedInstancesValidator {
	return &CompletedInstancesValidator{}
}

func (v *CompletedInstancesValidator) Name() string {
	return "completed-instances-consistency"
}

func (v *CompletedInstancesValidator) Severity() chaoskit.ValidationSeverity {
	return chaoskit.SeverityCritical
}

func (v *CompletedInstancesValidator) Validate(ctx context.Context, target chaoskit.Target) error {
	floxyTarget, ok := target.(*FloxyStressTarget)
	if !ok {
		return fmt.Errorf("target is not FloxyStressTarget")
	}

	// Find all completed instances that have steps not in completed state
	query := `
SELECT DISTINCT wi.id, wi.workflow_id
FROM workflows.workflow_instances wi
INNER JOIN workflows.workflow_steps ws ON wi.id = ws.instance_id
WHERE wi.status = 'completed'
  AND ws.status != 'completed'`

	rows, err := floxyTarget.pool.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to query completed instances: %w", err)
	}
	defer rows.Close()

	var violations []string
	for rows.Next() {
		var instanceID int64
		var workflowID string
		if err := rows.Scan(&instanceID, &workflowID); err != nil {
			return fmt.Errorf("failed to scan row: %w", err)
		}
		violations = append(violations, fmt.Sprintf("instance_id=%d workflow_id=%s", instanceID, workflowID))
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating rows: %w", err)
	}

	if len(violations) > 0 {
		return fmt.Errorf("found %d completed instances with non-completed steps: %v", len(violations), violations)
	}

	return nil
}

// FailedInstancesValidator checks step states for failed instances
type FailedInstancesValidator struct{}

func NewFailedInstancesValidator() *FailedInstancesValidator {
	return &FailedInstancesValidator{}
}

func (v *FailedInstancesValidator) Name() string {
	return "failed-instances-consistency"
}

func (v *FailedInstancesValidator) Severity() chaoskit.ValidationSeverity {
	return chaoskit.SeverityCritical
}

func (v *FailedInstancesValidator) Validate(ctx context.Context, target chaoskit.Target) error {
	floxyTarget, ok := target.(*FloxyStressTarget)
	if !ok {
		return fmt.Errorf("target is not FloxyStressTarget")
	}

	// Get all failed instances
	instancesQuery := `
SELECT id, workflow_id
FROM workflows.workflow_instances
WHERE status = 'failed'`

	instancesRows, err := floxyTarget.pool.Query(ctx, instancesQuery)
	if err != nil {
		return fmt.Errorf("failed to query failed instances: %w", err)
	}
	defer instancesRows.Close()

	var violations []string

	for instancesRows.Next() {
		var instanceID int64
		var workflowID string
		if err := instancesRows.Scan(&instanceID, &workflowID); err != nil {
			return fmt.Errorf("failed to scan instance row: %w", err)
		}

		// Check if there's a savepoint for this instance
		var savepointID *int64
		var savepointCreatedAt *string
		savepointQuery := `
			SELECT id, created_at::text
			FROM workflows.workflow_steps
			WHERE instance_id = $1 AND step_type = 'save_point'
			ORDER BY created_at DESC
			LIMIT 1`
		err := floxyTarget.pool.QueryRow(ctx, savepointQuery, instanceID).Scan(&savepointID, &savepointCreatedAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("failed to query savepoint for instance %d: %w", instanceID, err)
		}

		if savepointID == nil {
			// No savepoint - all steps must be in the rolled_back state
			nonRolledBackQuery := `
				SELECT id, step_name, status
				FROM workflows.workflow_steps
				WHERE instance_id = $1 AND status != 'rolled_back'
			`
			stepsRows, err := floxyTarget.pool.Query(ctx, nonRolledBackQuery, instanceID)
			if err != nil {
				return fmt.Errorf("failed to query steps for instance %d: %w", instanceID, err)
			}

			var nonRolledBackSteps []string
			for stepsRows.Next() {
				var stepID int64
				var stepName, status string
				if err := stepsRows.Scan(&stepID, &stepName, &status); err != nil {
					stepsRows.Close()
					return fmt.Errorf("failed to scan step row: %w", err)
				}
				nonRolledBackSteps = append(nonRolledBackSteps, fmt.Sprintf("step_id=%d step_name=%s status=%s", stepID, stepName, status))
			}
			stepsRows.Close()

			if err := stepsRows.Err(); err != nil {
				return fmt.Errorf("error iterating steps: %w", err)
			}

			if len(nonRolledBackSteps) > 0 {
				violations = append(violations, fmt.Sprintf("instance_id=%d workflow_id=%s (no savepoint): non-rolled_back steps: %v", instanceID, workflowID, nonRolledBackSteps))
			}
		} else {
			// Savepoint exists - check that all steps except savepoint and steps before savepoint are in rolled_back state
			// Steps that should be rolled_back:
			// - All steps created AFTER savepoint (created_at > savepoint.created_at)
			// - Steps created BEFORE savepoint, but not the savepoint itself
			nonRolledBackQuery := `
				SELECT ws.id, ws.step_name, ws.status, ws.created_at::text
				FROM workflows.workflow_steps ws
				WHERE ws.instance_id = $1
				  AND ws.status != 'rolled_back'
				  AND (
					-- Steps created after savepoint
					ws.created_at > (SELECT created_at FROM workflows.workflow_steps WHERE id = $2)
					OR
					-- Steps created before savepoint (but not the savepoint itself)
					(ws.created_at < (SELECT created_at FROM workflows.workflow_steps WHERE id = $2) AND ws.id != $2)
				  )`
			stepsRows, err := floxyTarget.pool.Query(ctx, nonRolledBackQuery, instanceID, *savepointID)
			if err != nil {
				return fmt.Errorf("failed to query steps for instance %d: %w", instanceID, err)
			}

			var nonRolledBackSteps []string
			for stepsRows.Next() {
				var stepID int64
				var stepName, status, createdAt string
				if err := stepsRows.Scan(&stepID, &stepName, &status, &createdAt); err != nil {
					stepsRows.Close()
					return fmt.Errorf("failed to scan step row: %w", err)
				}
				nonRolledBackSteps = append(nonRolledBackSteps, fmt.Sprintf("step_id=%d step_name=%s status=%s created_at=%s", stepID, stepName, status, createdAt))
			}
			stepsRows.Close()

			if err := stepsRows.Err(); err != nil {
				return fmt.Errorf("error iterating steps: %w", err)
			}

			if len(nonRolledBackSteps) > 0 {
				violations = append(violations, fmt.Sprintf("instance_id=%d workflow_id=%s (with savepoint_id=%d): non-rolled_back steps: %v", instanceID, workflowID, *savepointID, nonRolledBackSteps))
			}
		}
	}

	if err := instancesRows.Err(); err != nil {
		return fmt.Errorf("error iterating instances: %w", err)
	}

	if len(violations) > 0 {
		return fmt.Errorf("found %d failed instances with inconsistent step states:\n%v", len(violations), violations)
	}

	return nil
}
