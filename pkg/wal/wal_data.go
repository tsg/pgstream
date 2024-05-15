// SPDX-License-Identifier: Apache-2.0

package wal

import (
	"github.com/rs/xid"
)

type Data struct {
	Action    string   `json:"action"`    // "I" -- insert, "U" -- update, "D" -- delete, "T" -- truncate
	Timestamp string   `json:"timestamp"` // ISO8601, i.e. 2019-12-29 04:58:34.806671
	LSN       string   `json:"lsn"`
	Schema    string   `json:"schema"`
	Table     string   `json:"table"`
	Columns   []Column `json:"columns"`
	Identity  []Column `json:"identity"`
	Metadata  Metadata `json:"metadata"` // pgstream specific metadata
}

type Metadata struct {
	SchemaID        xid.ID `json:"schema_id"`         // the schema ID the event was stamped with
	TablePgstreamID string `json:"table_pgstream_id"` // the ID of the table to which the event belongs
	// This is the Pgstream ID of the "id" column. We track this specifically, as we extract it from the event
	// in order to use as the ID for the OS record.
	InternalColID string `json:"id_col_pgstream_id"`
	// This is the Pgstream ID of the "version" column. We track this specifically, as we extract it from the event
	// in order to use as the version when working with OS' optimistic concurrency checks.
	InternalColVersion string `json:"version_col_pgstream_id"`
}

type Column struct {
	ID    string `json:"id"` // xata assigned immutable column id. Id does not change when column is renamed.
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// IsEmpty is true if string fields are empty
func (m Metadata) IsEmpty() bool {
	if m.TablePgstreamID == "" && m.InternalColID == "" && m.InternalColVersion == "" {
		return true
	}
	return false
}
