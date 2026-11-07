package pluginhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrRevision = errors.New("record revision changed")
var ErrRequestConflict = errors.New("request key already bound to another invocation")
var applicationJobID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

const applicationJobPrefix = "application/"

// CreateJob binds an idempotency key to the complete immutable request, including
// source versions, plugin bytes and configuration revision. It never submits work.
func (s *Store) CreateJob(ctx context.Context, input Job) (Job, bool, error) {
	if strings.HasPrefix(input.Plugin, applicationJobPrefix) {
		return Job{}, false, errors.New("application jobs require the trusted application host")
	}
	return s.createJob(ctx, input, false)
}

// CreateApplicationJob creates a durable action on behalf of a registered
// application. The caller is the trusted station host: unlike a project-tool
// job, it is not backed by an installed plugin record. The application ID is
// still part of the idempotency identity and its adapter/authorization version
// fields are immutable after the first invocation.
func (s *Store) CreateApplicationJob(ctx context.Context, appID string, input Job) (Job, bool, error) {
	if !applicationJobID.MatchString(appID) || input.Plugin != "" && input.Plugin != applicationJobPrefix+appID ||
		strings.TrimSpace(input.Version) == "" || strings.TrimSpace(input.PackageDigest) == "" || strings.TrimSpace(input.ConfigRevision) == "" {
		return Job{}, false, errors.New("invalid application job invocation")
	}
	input.Plugin = applicationJobPrefix + appID
	return s.createJob(ctx, input, true)
}

func (s *Store) createJob(ctx context.Context, input Job, application bool) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var empty Job
	c := input.Context
	if c.Account != s.account || c.Thread == "" || c.Requester == "" || c.Purpose == "" || c.RequestKey == "" || input.Tool == "" || !json.Valid(input.Arguments) {
		return empty, false, errors.New("invalid job invocation")
	}
	// Canonical JSON makes retries with only object-key ordering differences equal.
	var args any
	decoder := json.NewDecoder(bytes.NewReader(input.Arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return empty, false, err
	}
	input.Arguments, _ = json.Marshal(args)
	request := Job{Plugin: input.Plugin, Version: input.Version, PackageDigest: input.PackageDigest, ConfigRevision: input.ConfigRevision, Context: c, Tool: input.Tool, Arguments: input.Arguments}
	raw, _ := json.Marshal(request)
	fingerprint := sha256.Sum256(raw)
	fp := hex.EncodeToString(fingerprint[:])
	var previous []byte
	var oldFP string
	err := s.db.QueryRowContext(ctx, `SELECT record,fingerprint FROM jobs WHERE plugin=? AND thread=? AND request_key=?`, input.Plugin, c.Thread, c.RequestKey).Scan(&previous, &oldFP)
	if err == nil {
		if oldFP != fp {
			return empty, false, ErrRequestConflict
		}
		if json.Unmarshal(previous, &empty) != nil {
			return Job{}, false, errors.New("invalid job record")
		}
		return empty, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, false, err
	}
	if !application {
		var digest string
		var enabled, removed int
		var manifest []byte
		if err = s.db.QueryRowContext(ctx, `SELECT digest,enabled,removed,manifest FROM installed WHERE id=?`, input.Plugin).Scan(&digest, &enabled, &removed, &manifest); err != nil {
			return empty, false, err
		}
		var p Package
		if json.Unmarshal(manifest, &p) != nil {
			return empty, false, errors.New("invalid plugin registration")
		}
		if enabled == 0 || removed != 0 || digest != input.PackageDigest || p.Version != input.Version {
			return empty, false, errors.New("plugin disabled or version changed")
		}
		var configRevision int
		err = s.db.QueryRowContext(ctx, `SELECT revision FROM configurations WHERE plugin=?`, input.Plugin).Scan(&configRevision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return empty, false, err
		}
		if input.ConfigRevision != strconv.Itoa(configRevision) {
			return empty, false, errors.New("plugin configuration changed")
		}
	}
	idSum := sha256.Sum256([]byte(s.account + "\x00" + input.Plugin + "\x00" + c.Thread + "\x00" + c.RequestKey))
	request.ID = hex.EncodeToString(idSum[:])
	request.State = "pending"
	request.Revision = 1
	request.Created = time.Now().UTC().Format(time.RFC3339Nano)
	request.Updated = request.Created
	raw, _ = json.Marshal(request)
	_, err = s.db.ExecContext(ctx, `INSERT INTO jobs(id,plugin,thread,request_key,fingerprint,record,revision,state) VALUES(?,?,?,?,?,?,?,?)`, request.ID, request.Plugin, c.Thread, c.RequestKey, fp, raw, 1, request.State)
	return request, err == nil, err
}
func (s *Store) Job(ctx context.Context, id string) (Job, error) {
	var raw []byte
	var job Job
	err := s.db.QueryRowContext(ctx, `SELECT record FROM jobs WHERE id=?`, id).Scan(&raw)
	if err != nil {
		return job, err
	}
	if err = json.Unmarshal(raw, &job); err != nil {
		return Job{}, errors.New("invalid job record")
	}
	return job, nil
}

// JobByRequest reads the already persisted invocation without creating or
// claiming a job. The caller still checks the requester's source and grant.
func (s *Store) JobByRequest(ctx context.Context, plugin, thread, key string) (Job, error) {
	var raw []byte
	var job Job
	if err := s.db.QueryRowContext(ctx, `SELECT record FROM jobs WHERE plugin=? AND thread=? AND request_key=?`, plugin, thread, key).Scan(&raw); err != nil {
		return job, err
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		return Job{}, err
	}
	return job, nil
}

func (s *Store) mutateJob(ctx context.Context, id string, revision int, change func(*Job) error) (Job, error) {
	return s.mutateJobPrivate(ctx, id, revision, nil, change)
}

func (s *Store) mutateJobPrivate(ctx context.Context, id string, revision int, private json.RawMessage, change func(*Job) error) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(private) > 1<<20 || len(private) > 0 && !json.Valid(private) {
		return Job{}, errors.New("invalid private completion checkpoint")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	var job Job
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT record FROM jobs WHERE id=?`, id).Scan(&stored)
	if err != nil {
		return job, err
	}
	if json.Unmarshal(stored, &job) != nil {
		return Job{}, errors.New("invalid job record")
	}
	if job.Revision != revision {
		return job, ErrRevision
	}
	if err = change(&job); err != nil {
		return job, err
	}
	job.Revision++
	job.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(job)
	if err != nil {
		return job, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE jobs SET record=?,revision=?,state=? WHERE id=? AND revision=?`, raw, job.Revision, job.State, id, revision)
	if err != nil {
		return job, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return job, err
	}
	if n != 1 {
		return job, ErrRevision
	}
	if len(private) > 0 {
		if job.State != "completed" {
			return job, errors.New("private result requires completion")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO job_private_results(job_id,revision,payload) VALUES(?,?,?)`, id, job.Revision, []byte(private)); err != nil {
			return job, err
		}
	}
	return job, tx.Commit()
}
func (s *Store) ClaimJob(ctx context.Context, id string, revision int) (Job, error) {
	return s.ClaimJobWithSubmission(ctx, id, revision, nil)
}

// ClaimJobWithSubmission commits host-verified effective parameters before
// any backend submission. The request fingerprint remains the original intent.
func (s *Store) ClaimJobWithSubmission(ctx context.Context, id string, revision int, submission json.RawMessage) (Job, error) {
	if len(submission) > 1<<20 || len(submission) != 0 && !json.Valid(submission) {
		return Job{}, errors.New("invalid submission checkpoint")
	}
	return s.mutateJob(ctx, id, revision, func(j *Job) error {
		if j.State != "pending" || j.CancelRequested {
			return errors.New("job already claimed or cancelled")
		}
		j.State = "submitting"
		j.Submission = append(json.RawMessage(nil), submission...)
		return nil
	})
}

// AcknowledgeJob is called only with a verified provider response. A crash before
// this checkpoint leaves submitting; recovery must reconcile the original key.
func (s *Store) AcknowledgeJob(ctx context.Context, id string, revision int, remoteID string) (Job, error) {
	return s.mutateJob(ctx, id, revision, func(j *Job) error {
		if remoteID == "" || len(remoteID) > 1024 {
			return errors.New("invalid remote job id")
		}
		if j.State != "submitting" && j.State != "needs_review" {
			return errors.New("job not awaiting acknowledgement")
		}
		if j.RemoteID != "" && j.RemoteID != remoteID {
			return errors.New("remote identity changed")
		}
		j.RemoteID = remoteID
		j.State = "running"
		j.Diagnostic = nil // The previously uncertain submission is now verified.
		return nil
	})
}
func (s *Store) RequestJobCancel(ctx context.Context, id string, revision int) (Job, error) {
	return s.mutateJob(ctx, id, revision, func(j *Job) error {
		if j.Recovery() == "terminal" {
			return errors.New("job already finished")
		}
		j.CancelRequested = true
		if j.State == "pending" {
			j.State = "cancelled"
		}
		return nil
	})
}

// FailJobPreparation closes work that the host verified was never claimed for
// submission. The revision check prevents replacing a concurrent claim.
func (s *Store) FailJobPreparation(ctx context.Context, id string, revision int, stage JobFailureStage) (Job, error) {
	code := "backend_preparation_failed"
	switch stage {
	case JobFailureDispatch:
	case JobFailureAuthority:
		code = "source_authorization_changed"
	case JobFailureValidation:
		code = "recipe_validation_failed"
	default:
		return Job{}, errors.New("invalid job preparation stage")
	}
	return s.mutateJob(ctx, id, revision, func(j *Job) error {
		if j.State != "pending" {
			return errors.New("job already claimed")
		}
		j.State = "failed"
		j.Diagnostic = &JobDiagnostic{Stage: stage, Code: code}
		return nil
	})
}

// FinishJob is a host-only checkpoint after provider/result validation, never a
// public endpoint accepting an arbitrary plugin's claim about delivery.
func (s *Store) FinishJob(ctx context.Context, id string, revision int, state string, verifiedAssets []string) (Job, error) {
	return s.finishJob(ctx, id, revision, state, verifiedAssets, nil)
}

// ReviewJobFailure checkpoints an uncertain host invocation with a fixed safe
// diagnostic. Neither plugin output nor error.Error() is accepted by this API.
// It uses the same transaction and revision CAS as other job transitions.
func (s *Store) ReviewJobFailure(ctx context.Context, id string, revision int, stage JobFailureStage) (Job, error) {
	switch stage {
	case JobFailureDispatch, JobFailureAuthority, JobFailureValidation, JobFailureResultMode:
	default:
		return Job{}, errors.New("invalid host job failure stage")
	}
	return s.finishJob(ctx, id, revision, "needs_review", nil, &JobDiagnostic{Stage: stage, Code: "plugin_invocation_failed"})
}

func (s *Store) finishJob(ctx context.Context, id string, revision int, state string, verifiedAssets []string, diagnostic *JobDiagnostic) (Job, error) {
	return s.mutateJob(ctx, id, revision, func(j *Job) error {
		if j.Recovery() == "terminal" || j.State == "pending" {
			return errors.New("job not executing")
		}
		if state != "completed" && state != "failed" && state != "cancelled" && state != "needs_review" {
			return errors.New("invalid terminal job state")
		}
		if state != "completed" && len(verifiedAssets) > 0 {
			return errors.New("unfinished job cannot attach completion artifacts")
		}
		j.State = state
		j.Artifacts = append([]string{}, verifiedAssets...)
		if diagnostic != nil {
			j.Diagnostic = &JobDiagnostic{Stage: diagnostic.Stage, Code: diagnostic.Code}
		} else if state != "needs_review" {
			j.Diagnostic = nil
		}
		return nil
	})
}

type JobPage struct {
	Items   []Job `json:"items"`
	Next    int64 `json:"next"`
	HasMore bool  `json:"has_more"`
	Limit   int   `json:"limit"`
}

// CompleteJob stores only a host-validated, scoped result. This is generation
// completion, never proof of WeChat acceptance. A cancellation request may race
// a real result; retain both facts rather than claiming cancellation succeeded.
func (s *Store) CompleteJob(ctx context.Context, id string, revision int, assets []string, result json.RawMessage) (Job, error) {
	return s.CompleteJobWithPrivate(ctx, id, revision, assets, result, nil)
}

// CompleteJobWithPrivate atomically checkpoints the public result and a host-only
// handoff. Private data is never part of Job, task lists or plugin initialization.
func (s *Store) CompleteJobWithPrivate(ctx context.Context, id string, revision int, assets []string, result, private json.RawMessage) (Job, error) {
	return s.checkpointResult(ctx, id, revision, "completed", assets, result, private)
}

// FailJob records a verified failure response, unlike an uncertain transport
// outcome. It preserves diagnostic codes without replaying the original call.
func (s *Store) FailJob(ctx context.Context, id string, revision int, result json.RawMessage) (Job, error) {
	return s.checkpointResult(ctx, id, revision, "failed", nil, result, nil)
}
func (s *Store) checkpointResult(ctx context.Context, id string, revision int, state string, assets []string, result, private json.RawMessage) (Job, error) {
	return s.mutateJobPrivate(ctx, id, revision, private, func(j *Job) error {
		if j.Recovery() == "terminal" || j.State == "pending" || (len(result) > 0 && !json.Valid(result)) {
			return errors.New("invalid completion checkpoint")
		}
		j.State = state
		j.Artifacts = append([]string{}, assets...)
		j.Diagnostic = nil // A verified result replaces the uncertain-call diagnostic.
		j.Result = nil
		if len(result) > 0 {
			var compact bytes.Buffer
			if err := json.Compact(&compact, result); err != nil {
				return err
			}
			j.Result = append(json.RawMessage(nil), compact.Bytes()...)
		}
		return nil
	})
}

func (s *Store) Jobs(ctx context.Context, plugin string, after int64, limit int) (JobPage, error) {
	return s.jobs(ctx, plugin, "", after, limit)
}

// JobsForTool pages only durable records for a trusted host tool. The tool
// predicate is evaluated by SQLite before LIMIT, so separate application
// ledgers cannot create empty pages for one another.
func (s *Store) JobsForTool(ctx context.Context, plugin, tool string, after int64, limit int) (JobPage, error) {
	if strings.TrimSpace(tool) == "" {
		return JobPage{Items: []Job{}, Next: after, Limit: limit}, errors.New("tool required")
	}
	return s.jobs(ctx, plugin, tool, after, limit)
}

func (s *Store) jobs(ctx context.Context, plugin, tool string, after int64, limit int) (JobPage, error) {
	page := JobPage{Items: []Job{}, Next: after, Limit: limit}
	if limit < 1 || after < 0 {
		return page, errors.New("positive page size and nonnegative cursor required")
	}
	query := `SELECT seq,record FROM jobs WHERE plugin=? AND seq>?`
	args := []any{plugin, after}
	if tool != "" {
		query += ` AND json_extract(CAST(record AS TEXT),'$.tool')=?`
		args = append(args, tool)
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	// Only the selected plugin/tool page is loaded; no in-memory full-table filtering.
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var seq int64
		var raw []byte
		var j Job
		if err = rows.Scan(&seq, &raw); err != nil {
			rows.Close()
			return page, err
		}
		if json.Unmarshal(raw, &j) != nil {
			rows.Close()
			return page, errors.New("invalid job record")
		}
		page.Next = seq
		page.Items = append(page.Items, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	moreQuery := `SELECT EXISTS(SELECT 1 FROM jobs WHERE plugin=? AND seq>?`
	moreArgs := []any{plugin, page.Next}
	if tool != "" {
		moreQuery += ` AND json_extract(CAST(record AS TEXT),'$.tool')=?`
		moreArgs = append(moreArgs, tool)
	}
	moreQuery += `)`
	var more int
	err = s.db.QueryRowContext(ctx, moreQuery, moreArgs...).Scan(&more)
	page.HasMore = more != 0
	return page, err
}

// HasUnfinishedJobs is a narrow lifecycle guard: callers learn only whether a
// plugin has work whose outcome still needs recovery, never another request's
// arguments, result, identity or diagnostic.  An uncertain submission is kept
// live until explicitly terminal rather than being replayed after a process
// recycle.
func (s *Store) HasUnfinishedJobs(ctx context.Context, plugin string) (bool, error) {
	if plugin == "" {
		return false, errors.New("plugin required")
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE plugin=? AND state NOT IN ('completed','failed','cancelled'))`, plugin).Scan(&exists)
	return exists != 0, err
}
