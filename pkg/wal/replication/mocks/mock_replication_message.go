// SPDX-License-Identifier: Apache-2.0

package mocks

import (
	"github.com/xataio/pgstream/pkg/wal/replication"
)

type Message struct {
	GetDataFn func() *replication.MessageData
}

func (m *Message) GetData() *replication.MessageData {
	return m.GetDataFn()
}