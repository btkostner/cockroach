// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package jobs

import (
	"bytes"
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
)

// InfoStorage can be used to read and write rows to system.jobs_info table. All
// operations are scoped under the txn and are are executed on behalf of Job j.
type InfoStorage struct {
	j   *Job
	txn isql.Txn
}

// InfoStorage returns a new InfoStorage with the passed in job and txn.
func (j *Job) InfoStorage(txn isql.Txn) InfoStorage {
	return InfoStorage{j: j, txn: txn}
}

func (i InfoStorage) checkClaimSession(ctx context.Context) error {
	row, err := i.txn.QueryRowEx(ctx, "check-claim-session", i.txn.KV(),
		sessiondata.NodeUserSessionDataOverride,
		`SELECT claim_session_id FROM system.jobs WHERE id = $1`, i.j.ID())
	if err != nil {
		return err
	}

	if row == nil {
		return errors.Errorf(
			"expected session %q for job ID %d but found none", i.j.Session().ID(), i.j.ID())
	}

	storedSession := []byte(*row[0].(*tree.DBytes))
	if !bytes.Equal(storedSession, i.j.Session().ID().UnsafeBytes()) {
		return errors.Errorf(
			"expected session %q but found %q", i.j.Session().ID(), sqlliveness.SessionID(storedSession))
	}

	return nil
}

func (i InfoStorage) get(ctx context.Context, infoKey []byte) ([]byte, bool, error) {
	if i.txn == nil {
		return nil, false, errors.New("cannot access the job info table without an associated txn")
	}

	ctx, sp := tracing.ChildSpan(ctx, "get-job-info")
	defer sp.Finish()

	j := i.j
	row, err := i.txn.QueryRowEx(
		ctx, "job-info-get", i.txn.KV(),
		sessiondata.NodeUserSessionDataOverride,
		"SELECT value FROM system.job_info WHERE job_id = $1 AND info_key = $2 ORDER BY written DESC LIMIT 1",
		j.ID(), infoKey,
	)

	if err != nil {
		return nil, false, err
	}

	if row == nil {
		return nil, false, nil
	}

	value, ok := row[0].(*tree.DBytes)
	if !ok {
		return nil, false, errors.AssertionFailedf("job info: expected value to be DBytes (was %T)", row[0])
	}

	return []byte(*value), true, nil
}

func (i InfoStorage) write(ctx context.Context, infoKey, value []byte) error {
	if i.txn == nil {
		return errors.New("cannot write to the job info table without an associated txn")
	}

	ctx, sp := tracing.ChildSpan(ctx, "write-job-info")
	defer sp.Finish()

	j := i.j

	if j.Session() != nil {
		if err := i.checkClaimSession(ctx); err != nil {
			return err
		}
	} else {
		log.VInfof(ctx, 1, "job %d: writing to the system.job_info with no session ID", j.ID())
	}

	// First clear out any older revisions of this info.
	_, err := i.txn.ExecEx(
		ctx, "write-job-info-delete", i.txn.KV(),
		sessiondata.NodeUserSessionDataOverride,
		"DELETE FROM system.job_info WHERE job_id = $1 AND info_key = $2",
		j.ID(), infoKey,
	)
	if err != nil {
		return err
	}

	// Write the new info, using the same transaction.
	_, err = i.txn.ExecEx(
		ctx, "write-job-info-insert", i.txn.KV(),
		sessiondata.NodeUserSessionDataOverride,
		`INSERT INTO system.job_info (job_id, info_key, written, value) VALUES ($1, $2, now(), $3)`,
		j.ID(), infoKey, value,
	)
	return err
}

func (i InfoStorage) iterate(
	ctx context.Context, infoPrefix []byte, fn func(infoKey, value []byte) error,
) (retErr error) {
	if i.txn == nil {
		return errors.New("cannot iterate over the job info table without an associated txn")
	}

	// TODO(dt): verify this predicate hits the index.
	rows, err := i.txn.QueryIteratorEx(
		ctx, "job-info-iter", i.txn.KV(),
		sessiondata.NodeUserSessionDataOverride,
		`SELECT info_key, value 
		FROM system.job_info 
		WHERE job_id = $1 AND substring(info_key for $2) = $3 
		ORDER BY info_key ASC, written DESC`,
		i.j.ID(), len(infoPrefix), infoPrefix,
	)
	if err != nil {
		return err
	}
	defer func(it isql.Rows) { retErr = errors.CombineErrors(retErr, it.Close()) }(rows)

	var prevKey []byte
	var ok bool
	for ok, err = rows.Next(ctx); ok; ok, err = rows.Next(ctx) {
		if err != nil {
			return err
		}
		row := rows.Cur()

		key, ok := row[0].(*tree.DBytes)
		if !ok {
			return errors.AssertionFailedf("job info: expected info_key to be DBytes (was %T)", row[0])
		}
		infoKey := []byte(*key)

		if bytes.Equal(infoKey, prevKey) {
			continue
		}
		prevKey = append(prevKey[:0], infoKey...)

		value, ok := row[1].(*tree.DBytes)
		if !ok {
			return errors.AssertionFailedf("job info: expected value to be DBytes (was %T)", row[1])
		}
		if err = fn(infoKey, []byte(*value)); err != nil {
			return err
		}
	}

	return err
}

// Get fetches the latest info record for the given job and infoKey.
func (i InfoStorage) Get(ctx context.Context, infoKey []byte) ([]byte, bool, error) {
	return i.get(ctx, infoKey)
}

// Write writes the provided value to an info record for the provided jobID and
// infoKey after removing any existing info records for that job and infoKey
// using the same transaction, effectively replacing any older row with a row
// with the new value.
func (i InfoStorage) Write(ctx context.Context, infoKey, value []byte) error {
	return i.write(ctx, infoKey, value)
}

// Iterate iterates though the info records for a given job and info key prefix.
func (i InfoStorage) Iterate(
	ctx context.Context, infoPrefix []byte, fn func(infoKey, value []byte) error,
) (retErr error) {
	return i.iterate(ctx, infoPrefix, fn)
}

const (
	legacyPayloadKey  = "legacy_payload"
	legacyProgressKey = "legacy_progress"
)

// GetLegacyPayloadKey returns the info_key whose value is the jobspb.Payload of
// the job.
func GetLegacyPayloadKey() []byte {
	return []byte(legacyPayloadKey)
}

// GetLegacyProgressKey returns the info_key whose value is the jobspb.Progress
// of the job.
func GetLegacyProgressKey() []byte {
	return []byte(legacyProgressKey)
}

// GetLegacyPayload returns the job's Payload from the system.jobs_info table.
func (i InfoStorage) GetLegacyPayload(ctx context.Context) ([]byte, bool, error) {
	return i.Get(ctx, []byte(legacyPayloadKey))
}

// WriteLegacyPayload writes the job's Payload to the system.jobs_info table.
func (i InfoStorage) WriteLegacyPayload(ctx context.Context, payload []byte) error {
	return i.Write(ctx, []byte(legacyPayloadKey), payload)
}

// GetLegacyProgress returns the job's Progress from the system.jobs_info table.
func (i InfoStorage) GetLegacyProgress(ctx context.Context) ([]byte, bool, error) {
	return i.Get(ctx, []byte(legacyProgressKey))
}

// WriteLegacyProgress writes the job's Progress to the system.jobs_info table.
func (i InfoStorage) WriteLegacyProgress(ctx context.Context, progress []byte) error {
	return i.Write(ctx, []byte(legacyProgressKey), progress)
}
