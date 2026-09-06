package publicapp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Pippit-dev/pippit-cli/internal/mcpserver"
)

// GetJob is called through Execute so OAuth, rate, concurrency and audit checks
// remain the same as other reads. Neither the key nor a job ID grants ownership.
func (p *requestPolicy) GetJob(ctx context.Context, key string) (mcpserver.PublicJob, error) {
	var job mcpserver.PublicJob
	if len(key) < 8 || len(key) > 128 || strings.TrimSpace(key) != key {
		return job, nil
	}
	var submission, result []byte
	err := p.app.store.DB.QueryRowContext(ctx, `SELECT id,tool,state,COALESCE(thread_id,''),COALESCE(run_id,''),generation_finished,response,result_metadata FROM jobs WHERE user_id=$1 AND account_id=$2 AND idempotency_key=$3`, p.principal.UserID, p.accountID, key).Scan(&job.JobID, &job.Tool, &job.SubmissionState, &job.ThreadID, &job.RunID, &job.GenerationFinished, &submission, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return job, nil
	}
	if err != nil {
		return mcpserver.PublicJob{}, errors.New("job_metadata_unavailable")
	}
	for _, field := range []struct {
		raw []byte
		out *any
	}{{submission, &job.Submission}, {result, &job.Result}} {
		if len(field.raw) > 0 && json.Unmarshal(field.raw, field.out) != nil {
			return mcpserver.PublicJob{}, errors.New("invalid_job_metadata")
		}
	}
	job.Found = true
	// Keep recovery available even when two individually valid saved responses
	// exceed the MCP metadata limit together. Preserve IDs and the latest result.
	encoded, err := json.Marshal(job)
	if err != nil {
		return mcpserver.PublicJob{}, errors.New("invalid_job_metadata")
	}
	if len(encoded) > 2<<20 {
		job.MetadataOmitted = true
		job.Submission = nil
		encoded, _ = json.Marshal(job)
		if len(encoded) > 2<<20 {
			job.Result = nil
		}
	}
	return job, nil
}
