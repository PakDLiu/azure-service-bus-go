package servicebus

//	MIT License
//
//	Copyright (c) Microsoft Corporation. All rights reserved.
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-amqp-common-go/log"
	"github.com/Azure/azure-amqp-common-go/rpc"
	"github.com/Azure/azure-amqp-common-go/uuid"
	"github.com/Azure/go-autorest/autorest/date"
	"pack.ag/amqp"
)

type (
	entity struct {
		Name                  string
		namespace             *Namespace
		renewMessageLockMutex sync.Mutex
	}

	// Queue represents a Service Bus Queue entity, which offers First In, First Out (FIFO) message delivery to one or
	// more competing consumers. That is, messages are typically expected to be received and processed by the receivers
	// in the order in which they were added to the queue, and each message is received and processed by only one
	// message consumer.
	Queue struct {
		*entity
		sender            *sender
		senderMu          sync.Mutex
		receiveMode       ReceiveMode
		requiredSessionID *string
	}

	// queueContent is a specialized Queue body for an Atom entry
	queueContent struct {
		XMLName          xml.Name         `xml:"content"`
		Type             string           `xml:"type,attr"`
		QueueDescription QueueDescription `xml:"QueueDescription"`
	}

	// QueueDescription is the content type for Queue management requests
	QueueDescription struct {
		XMLName xml.Name `xml:"QueueDescription"`
		BaseEntityDescription
		LockDuration                        *string       `xml:"LockDuration,omitempty"`               // LockDuration - ISO 8601 timespan duration of a peek-lock; that is, the amount of time that the message is locked for other receivers. The maximum value for LockDuration is 5 minutes; the default value is 1 minute.
		MaxSizeInMegabytes                  *int32        `xml:"MaxSizeInMegabytes,omitempty"`         // MaxSizeInMegabytes - The maximum size of the queue in megabytes, which is the size of memory allocated for the queue. Default is 1024.
		RequiresDuplicateDetection          *bool         `xml:"RequiresDuplicateDetection,omitempty"` // RequiresDuplicateDetection - A value indicating if this queue requires duplicate detection.
		RequiresSession                     *bool         `xml:"RequiresSession,omitempty"`
		DefaultMessageTimeToLive            *string       `xml:"DefaultMessageTimeToLive,omitempty"`            // DefaultMessageTimeToLive - ISO 8601 default message timespan to live value. This is the duration after which the message expires, starting from when the message is sent to Service Bus. This is the default value used when TimeToLive is not set on a message itself.
		DeadLetteringOnMessageExpiration    *bool         `xml:"DeadLetteringOnMessageExpiration,omitempty"`    // DeadLetteringOnMessageExpiration - A value that indicates whether this queue has dead letter support when a message expires.
		DuplicateDetectionHistoryTimeWindow *string       `xml:"DuplicateDetectionHistoryTimeWindow,omitempty"` // DuplicateDetectionHistoryTimeWindow - ISO 8601 timeSpan structure that defines the duration of the duplicate detection history. The default value is 10 minutes.
		MaxDeliveryCount                    *int32        `xml:"MaxDeliveryCount,omitempty"`                    // MaxDeliveryCount - The maximum delivery count. A message is automatically deadlettered after this number of deliveries. default value is 10.
		EnableBatchedOperations             *bool         `xml:"EnableBatchedOperations,omitempty"`             // EnableBatchedOperations - Value that indicates whether server-side batched operations are enabled.
		SizeInBytes                         *int64        `xml:"SizeInBytes,omitempty"`                         // SizeInBytes - The size of the queue, in bytes.
		MessageCount                        *int64        `xml:"MessageCount,omitempty"`                        // MessageCount - The number of messages in the queue.
		IsAnonymousAccessible               *bool         `xml:"IsAnonymousAccessible,omitempty"`
		Status                              *EntityStatus `xml:"Status,omitempty"`
		CreatedAt                           *date.Time    `xml:"CreatedAt,omitempty"`
		UpdatedAt                           *date.Time    `xml:"UpdatedAt,omitempty"`
		SupportOrdering                     *bool         `xml:"SupportOrdering,omitempty"`
		AutoDeleteOnIdle                    *string       `xml:"AutoDeleteOnIdle,omitempty"`
		EnablePartitioning                  *bool         `xml:"EnablePartitioning,omitempty"`
		EnableExpress                       *bool         `xml:"EnableExpress,omitempty"`
		CountDetails                        *CountDetails `xml:"CountDetails,omitempty"`
	}

	// QueueOption represents named options for assisting Queue message handling
	QueueOption func(*Queue) error

	// ReceiveMode represents the behavior when consuming a message from a queue
	ReceiveMode int

	closer interface {
		Close(context.Context) error
	}
)

const (
	// PeekLockMode causes a receiver to peek at a message, lock it so no others can consume and have the queue wait for
	// the DispositionAction
	PeekLockMode ReceiveMode = 0
	// ReceiveAndDeleteMode causes a receiver to pop messages off of the queue without waiting for DispositionAction
	ReceiveAndDeleteMode ReceiveMode = 1
)

// QueueWithReceiveAndDelete configures a queue to pop and delete messages off of the queue upon receiving the message.
// This differs from the default, PeekLock, where PeekLock receives a message, locks it for a period of time, then sends
// a disposition to the broker when the message has been processed.
func QueueWithReceiveAndDelete() QueueOption {
	return func(q *Queue) error {
		q.receiveMode = ReceiveAndDeleteMode
		return nil
	}
}

//// QueueWithRequiredSession configures a queue to use a session
//func QueueWithRequiredSession(sessionID string) QueueOption {
//	return func(q *Queue) error {
//		q.requiredSessionID = &sessionID
//		return nil
//	}
//}

// NewQueue creates a new Queue Sender / Receiver
func (ns *Namespace) NewQueue(name string, opts ...QueueOption) (*Queue, error) {
	queue := &Queue{
		entity: &entity{
			namespace: ns,
			Name:      name,
		},
		receiveMode: PeekLockMode,
	}

	for _, opt := range opts {
		if err := opt(queue); err != nil {
			return nil, err
		}
	}
	return queue, nil
}

// Send sends messages to the Queue
func (q *Queue) Send(ctx context.Context, event *Message) error {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.Send")
	defer span.Finish()

	err := q.ensureSender(ctx)
	if err != nil {
		log.For(ctx).Error(err)
		return err
	}
	return q.sender.Send(ctx, event)
}

// ScheduleAt will send a batch of messages to a Queue, schedule them to be enqueued, and return the sequence numbers
// that can be used to cancel each message.
func (q *Queue) ScheduleAt(ctx context.Context, enqueueTime time.Time, messages ...*Message) ([]int64, error) {
	if len(messages) <= 0 {
		return nil, errors.New("expected one or more messages")
	}

	transformed := make([]interface{}, 0, len(messages))
	for i := range messages {
		messages[i].ScheduleAt(enqueueTime)

		if messages[i].ID == "" {
			id, err := uuid.NewV4()
			if err != nil {
				return nil, err
			}
			messages[i].ID = id.String()
		}

		rawAmqp, err := messages[i].toMsg()
		if err != nil {
			return nil, err
		}
		encoded, err := rawAmqp.MarshalBinary()
		if err != nil {
			return nil, err
		}

		individualMessage := map[string]interface{}{
			"message-id": messages[i].ID,
			"message":    encoded,
		}
		if messages[i].GroupID != nil {
			individualMessage["session-id"] = *messages[i].GroupID
		}
		if partitionKey := messages[i].SystemProperties.PartitionKey; partitionKey != nil {
			individualMessage["partition-key"] = *partitionKey
		}
		if viaPartitionKey := messages[i].SystemProperties.ViaPartitionKey; viaPartitionKey != nil {
			individualMessage["via-partition-key"] = *viaPartitionKey
		}

		transformed = append(transformed, individualMessage)
	}

	msg := &amqp.Message{
		ApplicationProperties: map[string]interface{}{
			operationFieldName: scheduleMessageOperationID,
		},
		Value: map[string]interface{}{
			"messages": transformed,
		},
	}

	if deadline, ok := ctx.Deadline(); ok {
		msg.ApplicationProperties[serverTimeoutFieldName] = uint(time.Until(deadline) / time.Millisecond)
	}

	err := q.ensureSender(ctx)
	if err != nil {
		return nil, err
	}

	link, err := rpc.NewLink(q.sender.connection, q.ManagementPath())
	if err != nil {
		return nil, err
	}

	resp, err := link.RetryableRPC(ctx, 5, 5*time.Second, msg)
	if err != nil {
		return nil, err
	}

	if resp.Code != 200 {
		return nil, ErrAMQP(*resp)
	}

	retval := make([]int64, 0, len(messages))

	if rawVal, ok := resp.Message.Value.(map[string]interface{}); ok {
		const sequenceFieldName = "sequence-numbers"
		if rawArr, ok := rawVal[sequenceFieldName]; ok {
			if arr, ok := rawArr.([]int64); ok {
				for i := range arr {
					retval = append(retval, arr[i])
				}
				return retval, nil
			}
			return nil, newErrIncorrectType(sequenceFieldName, []int64{}, rawArr)
		}
		return nil, ErrMissingField(sequenceFieldName)
	}
	return nil, newErrIncorrectType("value", map[string]interface{}{}, resp.Message.Value)
}

// CancelScheduled allows for removal of messages that have been handed to the Service Bus broker for later delivery,
// but have not yet ben enqueued.
func (q *Queue) CancelScheduled(ctx context.Context, seq ...int64) error {
	msg := &amqp.Message{
		ApplicationProperties: map[string]interface{}{
			operationFieldName: cancelScheduledOperationID,
		},
		Value: map[string]interface{}{
			"sequence-numbers": seq,
		},
	}

	if deadline, ok := ctx.Deadline(); ok {
		msg.ApplicationProperties[serverTimeoutFieldName] = uint(time.Until(deadline) / time.Millisecond)
	}

	err := q.ensureSender(ctx)
	if err != nil {
		return err
	}

	link, err := rpc.NewLink(q.sender.connection, q.ManagementPath())
	if err != nil {
		return err
	}

	resp, err := link.RetryableRPC(ctx, 5, 5*time.Second, msg)
	if err != nil {
		return err
	}

	if resp.Code != 200 {
		return ErrAMQP(*resp)
	}

	return nil
}

// Peek fetches a list of Messages from the Service Bus broker without acquiring a lock or committing to a disposition.
// The messages are delivered as close to sequence order as possible.
//
// The MessageIterator that is returned has the following properties:
// - Messages are fetches from the server in pages. Page size is configurable with PeekOptions.
// - The MessageIterator will always return "false" for Done().
// - When Next() is called, it will return either: a slice of messages and no error, nil with an error related to being
// unable to complete the operation, or an empty slice of messages and an instance of "ErrNoMessages" signifying that
// there are currently no messages in the queue with a sequence ID larger than previously viewed ones.
func (q *Queue) Peek(ctx context.Context, options ...PeekOption) (MessageIterator, error) {
	c, err := q.namespace.connection()
	if err != nil {
		return nil, err
	}

	return newPeekIterator(q.entity, c, options...)
}

// PeekOne fetches a single Message from the Service Bus broker without acquiring a lock or committing to a disposition.
func (q *Queue) PeekOne(ctx context.Context, options ...PeekOption) (*Message, error) {
	c, err := q.namespace.connection()
	if err != nil {
		return nil, err
	}

	// Adding PeekWithPageSize(1) as the last option assures that either:
	// - creating the iterator will fail because two of the same option will be applied.
	// - PeekWithPageSize(1) will be applied after all others, so we will not wastefully pull down messages destined to
	//   be unread.
	options = append(options, PeekWithPageSize(1))

	it, err := newPeekIterator(q.entity, c, options...)
	if err != nil {
		return nil, err
	}
	return it.Next(ctx)
}

// ReceiveOne will listen to receive a single message. ReceiveOne will only wait as long as the context allows.
func (q *Queue) ReceiveOne(ctx context.Context, handler Handler) error {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.ReceiveOne")
	defer span.Finish()

	r, err := q.newReceiver(ctx)
	if err != nil {
		return err
	}
	defer closeLink(ctx, r)

	return r.ReceiveOne(ctx, handler)
}

// Receive subscribes for messages sent to the Queue. If the messages not within a session, messages will arrive
// unordered.
func (q *Queue) Receive(ctx context.Context, handler Handler) error {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.Receive")
	defer span.Finish()

	r, err := q.newReceiver(ctx)
	if err != nil {
		return err
	}
	defer closeLink(ctx, r)

	handle := r.Listen(ctx, handler)
	<-handle.Done()
	return handle.Err()
}

// ReceiveOneSession waits for the lock on a particular session to become available, takes it, then process the session.
// The session can contain multiple messages. ReceiveOneSession will receive all messages within that session.
func (q *Queue) ReceiveOneSession(ctx context.Context, sessionID *string, handler SessionHandler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.ReceiveOneSession")
	defer span.Finish()

	r, err := q.newReceiver(ctx, receiverWithSession(sessionID))
	if err != nil {
		return err
	}
	defer closeLink(ctx, r)

	ms, err := newMessageSession(r, q.entity, sessionID)
	if err != nil {
		return err
	}

	err = handler.Start(ms)
	if err != nil {
		return err
	}

	defer handler.End()
	handle := r.Listen(ctx, handler)

	select {
	case <-handle.Done():
		return handle.Err()
	case <-ms.done:
		return nil
	}
}

// ReceiveSessions is the session-based counterpart of `Receive`. It subscribes to a Queue and waits for new sessions to
// become available.
func (q *Queue) ReceiveSessions(ctx context.Context, handler SessionHandler) error {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.ReceiveSessions")
	defer span.Finish()

	for {
		if err := q.ReceiveOneSession(ctx, nil, handler); err != nil {
			return err
		}
	}
}

func (q *Queue) newReceiver(ctx context.Context, opts ...receiverOption) (*receiver, error) {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.newReceiver")
	defer span.Finish()

	opts = append(opts, receiverWithReceiveMode(q.receiveMode))
	r, err := q.namespace.newReceiver(ctx, q.Name, opts...)
	if err != nil {
		log.For(ctx).Error(err)
		return r, err
	}

	return r, nil
}

// Close the underlying connection to Service Bus
func (q *Queue) Close(ctx context.Context) error {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.Close")
	defer span.Finish()

	if q.sender != nil {
		return q.sender.Close(ctx)
	}

	return nil
}

func (q *Queue) ensureSender(ctx context.Context) error {
	span, ctx := q.startSpanFromContext(ctx, "sb.Queue.ensureSender")
	defer span.Finish()

	q.senderMu.Lock()
	defer q.senderMu.Unlock()

	if q.sender == nil {
		s, err := q.namespace.newSender(ctx, q.Name)
		if err != nil {
			log.For(ctx).Error(err)
			return err
		}
		q.sender = s
	}
	return nil
}

func closeLink(ctx context.Context, c closer) {
	err := c.Close(ctx)
	if err != nil {
		log.For(ctx).Error(err)
	}
}

func (e *entity) ManagementPath() string {
	return fmt.Sprintf("%s/$management", e.Name)
}
