package input

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Jeffail/benthos/v3/internal/checkpoint"
	"github.com/Jeffail/benthos/v3/lib/message"
	"github.com/Jeffail/benthos/v3/lib/message/batch"
	"github.com/Jeffail/benthos/v3/lib/types"
	"github.com/aws/aws-sdk-go/service/kinesis"
)

type awsKinesisRecordBatcher struct {
	streamID string
	shardID  string

	batchPolicy  *batch.Policy
	checkpointer *checkpoint.Capped

	flushedMessage types.Message

	batchedSequence string
	batchedIndex    int

	ackedSequence   string
	indexToSequence map[int]string
	ackedMut        sync.Mutex
	ackedWG         sync.WaitGroup
}

func (k *kinesisReader) newAWSKinesisRecordBatcher(streamID, shardID, sequence string) (*awsKinesisRecordBatcher, error) {
	batchPolicy, err := batch.NewPolicy(k.conf.Batching, k.mgr, k.log, k.stats)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize batch policy for shard consumer: %w", err)
	}

	return &awsKinesisRecordBatcher{
		streamID:        streamID,
		shardID:         shardID,
		batchPolicy:     batchPolicy,
		checkpointer:    checkpoint.NewCapped(k.conf.CheckpointLimit),
		ackedSequence:   sequence,
		indexToSequence: map[int]string{},
	}, nil
}

func (a *awsKinesisRecordBatcher) AddRecord(r *kinesis.Record) bool {
	p := message.NewPart(r.Data)
	met := p.Metadata()
	met.Set("kinesis_stream", a.streamID)
	met.Set("kinesis_shard", a.shardID)
	if r.PartitionKey != nil {
		met.Set("kinesis_partition_key", *r.PartitionKey)
	}
	met.Set("kinesis_sequence_number", *r.SequenceNumber)

	a.batchedSequence = *r.SequenceNumber
	a.batchedIndex++

	if a.flushedMessage != nil {
		// Upstream shouldn't really be adding records if a prior flush was
		// unsuccessful. However, we can still accommodate this by appending it
		// to the flushed message.
		a.flushedMessage.Append(p)
		return true
	}
	return a.batchPolicy.Add(p)
}

func (a *awsKinesisRecordBatcher) HasPendingMessage() bool {
	return a.flushedMessage != nil
}

func (a *awsKinesisRecordBatcher) FlushMessage(ctx context.Context) (asyncMessage, error) {
	if a.flushedMessage == nil {
		if a.flushedMessage = a.batchPolicy.Flush(); a.flushedMessage == nil {
			return asyncMessage{}, nil
		}
	}

	if err := a.checkpointer.Track(ctx, a.batchedIndex); err != nil {
		if err == types.ErrTimeout {
			err = nil
		}
		return asyncMessage{}, err
	}

	index := a.batchedIndex

	a.ackedMut.Lock()
	a.indexToSequence[a.batchedIndex] = a.batchedSequence
	a.ackedMut.Unlock()
	a.ackedWG.Add(1)

	aMsg := asyncMessage{
		msg: a.flushedMessage,
		ackFn: func(ctx context.Context, res types.Response) error {
			topIndex, err := a.checkpointer.Resolve(index)

			a.ackedMut.Lock()
			if err == nil {
				if seq, exists := a.indexToSequence[topIndex]; exists {
					a.ackedSequence = seq
					for k := range a.indexToSequence {
						if k <= topIndex {
							delete(a.indexToSequence, topIndex)
						}
					}
				}
			}
			a.ackedMut.Unlock()

			a.ackedWG.Done()
			return err
		},
	}
	a.flushedMessage = nil
	return aMsg, nil
}

func (a *awsKinesisRecordBatcher) UntilNext() time.Duration {
	return a.batchPolicy.UntilNext()
}

func (a *awsKinesisRecordBatcher) GetSequence() string {
	a.ackedMut.Lock()
	seq := a.ackedSequence
	a.ackedMut.Unlock()
	return seq
}

func (a *awsKinesisRecordBatcher) Close(blocked bool) {
	if blocked {
		a.ackedWG.Wait()
	}
	a.batchPolicy.CloseAsync()
}
