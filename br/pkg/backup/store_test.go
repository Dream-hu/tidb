// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backup

import (
	"context"
	"testing"
	"time"

	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type MockBackupClient struct {
	backuppb.BackupClient

	recvFunc func(context.Context) (*backuppb.BackupResponse, error)
}

func (mbc *MockBackupClient) Backup(ctx context.Context, _ *backuppb.BackupRequest, _ ...grpc.CallOption) (backuppb.Backup_BackupClient, error) {
	return &MockBackupBackupClient{ctx: ctx, recvFunc: mbc.recvFunc}, nil
}

type MockBackupBackupClient struct {
	backuppb.Backup_BackupClient

	ctx      context.Context
	recvFunc func(context.Context) (*backuppb.BackupResponse, error)
}

func (mbbc *MockBackupBackupClient) CloseSend() error {
	return nil
}

func (mbbc *MockBackupBackupClient) Recv() (*backuppb.BackupResponse, error) {
	if mbbc.recvFunc != nil {
		return mbbc.recvFunc(mbbc.ctx)
	}
	return &backuppb.BackupResponse{}, nil
}

func TestTimeoutRecv(t *testing.T) {
	ctx := context.Background()
	TimeoutOneResponse = time.Millisecond * 800
	// Just Timeout Once
	{
		err := startBackup(ctx, 0, NewResourceMemoryLimiter(100), backuppb.BackupRequest{}, &MockBackupClient{
			recvFunc: func(ctx context.Context) (*backuppb.BackupResponse, error) {
				time.Sleep(time.Second)
				require.Error(t, ctx.Err())
				return nil, ctx.Err()
			},
		}, 1, nil)
		require.Error(t, err)
	}

	// Timeout Not At First
	{
		count := 0
		err := startBackup(ctx, 0, NewResourceMemoryLimiter(100), backuppb.BackupRequest{}, &MockBackupClient{
			recvFunc: func(ctx context.Context) (*backuppb.BackupResponse, error) {
				require.NoError(t, ctx.Err())
				if count == 15 {
					time.Sleep(time.Second)
					require.Error(t, ctx.Err())
					return nil, ctx.Err()
				}
				count += 1
				time.Sleep(time.Millisecond * 80)
				return &backuppb.BackupResponse{}, nil
			},
		}, 1, make(chan *ResponseAndStore, 15))
		require.Error(t, err)
		require.Equal(t, count, 15)
	}
}

func TestTimeoutRecvCancel(t *testing.T) {
	ctx := context.Background()
	cctx, cancel := context.WithCancel(ctx)

	_, trecv := StartTimeoutRecv(cctx, time.Hour, 0)
	cancel()
	trecv.wg.Wait()
}

func TestTimeoutRecvCanceled(t *testing.T) {
	ctx := context.Background()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tctx, trecv := StartTimeoutRecv(cctx, time.Hour, 0)
	trecv.Stop()
	require.Equal(t, "context canceled", tctx.Err().Error())
}
