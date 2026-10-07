// Package queue is a durable job queue on Redis Streams with consumer groups and crash recovery
// (XAUTOCLAIM re-delivers messages whose consumer died before acknowledging).
package queue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	Stream  = "m365rescue:jobs"
	Group   = "workers"
	minIdle = 15 * time.Second
)

type Queue struct{ R *redis.Client }

func (q *Queue) Init(ctx context.Context) error {
	err := q.R.XGroupCreateMkStream(ctx, Stream, Group, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return err
	}
	return nil
}

func (q *Queue) Enqueue(ctx context.Context, jobID string) error {
	return q.R.XAdd(ctx, &redis.XAddArgs{Stream: Stream, Values: map[string]any{"job_id": jobID}}).Err()
}

func (q *Queue) Depth(ctx context.Context) int64 {
	n, _ := q.R.XLen(ctx, Stream).Result()
	return n
}

// Handler returns ack=true when the message is finished (success OR permanently failed / obsolete).
// ack=false leaves the message pending so it is re-delivered after minIdle.
type Handler func(ctx context.Context, jobID string) (ack bool, err error)

func (q *Queue) Run(ctx context.Context, name string, concurrency int, h Handler) {
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(consumer string) {
			defer wg.Done()
			q.loop(ctx, consumer, h)
		}(fmt.Sprintf("%s-%d", name, i))
	}
	wg.Wait()
}

func (q *Queue) loop(ctx context.Context, consumer string, h Handler) {
	for ctx.Err() == nil {
		msgs, _, err := q.R.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: Stream, Group: Group, Consumer: consumer, MinIdle: minIdle, Start: "0-0", Count: 1}).Result()
		if err != nil && ctx.Err() == nil {
			log.Printf("queue: autoclaim: %v", err)
		}
		if len(msgs) == 0 {
			streams, err := q.R.XReadGroup(ctx, &redis.XReadGroupArgs{Group: Group, Consumer: consumer, Streams: []string{Stream, ">"}, Count: 1, Block: 2 * time.Second}).Result()
			if err != nil {
				if !errors.Is(err, redis.Nil) && ctx.Err() == nil {
					log.Printf("queue: read: %v", err)
					time.Sleep(time.Second)
				}
				continue
			}
			for _, s := range streams {
				msgs = append(msgs, s.Messages...)
			}
		}
		for _, m := range msgs {
			id, _ := m.Values["job_id"].(string)
			ack, err := h(ctx, id)
			if err != nil {
				log.Printf("job %s: %v", id, err)
			}
			if ack {
				q.R.XAck(context.Background(), Stream, Group, m.ID)
				q.R.XDel(context.Background(), Stream, m.ID)
			}
		}
	}
}
