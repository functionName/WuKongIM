package wkdb

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/trace"
	"github.com/WuKongIM/WuKongIM/pkg/wkdb/key"
	wkproto "github.com/WuKongIM/WuKongIMGoProto"
	"github.com/cockroachdb/pebble"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (wk *wukongDB) AppendMessages(channelId string, channelType uint8, msgs []Message) error {

	if wk.opts.EnableCost {
		start := time.Now()
		defer func() {
			cost := time.Since(start)
			if cost.Milliseconds() > 200 {
				wk.Info("appendMessages done", zap.Duration("cost", cost), zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Int("msgCount", len(msgs)))
			}
		}()
	}

	db := wk.channelDb(channelId, channelType)
	batch := db.NewBatch()
	defer batch.Close()
	for _, msg := range msgs {
		if err := wk.writeMessage(channelId, channelType, msg, batch); err != nil {
			return err
		}
		err := wk.setChannelLastMessageSeq(channelId, channelType, uint64(msg.MessageSeq), batch, wk.noSync)
		if err != nil {
			return err
		}
	}

	// 消息总数量+1
	err := wk.IncMessageCount(1)
	if err != nil {
		return err
	}

	return batch.Commit(wk.sync)
}

func (wk *wukongDB) channelDb(channelId string, channelType uint8) *pebble.DB {
	dbIndex := wk.channelDbIndex(channelId, channelType)
	return wk.shardDBById(uint32(dbIndex))
}

func (wk *wukongDB) channelDbIndex(channelId string, channelType uint8) uint32 {
	return uint32(key.ChannelIdToNum(channelId, channelType) % uint64(len(wk.dbs)))
}

func (wk *wukongDB) AppendMessagesBatch(reqs []AppendMessagesReq) error {

	// 监控
	trace.GlobalTrace.Metrics.DB().MessageAppendBatchCountAdd(1)

	// 按照db进行分组
	dbMap := make(map[uint32][]AppendMessagesReq)
	if wk.opts.EnableCost {
		start := time.Now()
		defer func() {
			cost := time.Since(start)
			if cost > time.Millisecond*1000 {
				msgCount := 0
				for _, req := range reqs {
					msgCount += len(req.Messages)
				}
				wk.Info("appendMessagesBatch done", zap.Duration("cost", cost), zap.Int("reqs", len(reqs)), zap.Int("msgCount", msgCount), zap.Int("dbCount", len(dbMap)))
			}
		}()
	}

	var msgTotalCount int
	for _, req := range reqs {
		shardId := wk.channelDbIndex(req.ChannelId, req.ChannelType)
		dbMap[shardId] = append(dbMap[shardId], req)
		msgTotalCount += len(req.Messages)
	}

	if len(dbMap) == 1 { // 如果只有一条消息 则不需要开启协程
		for shardId, reqs := range dbMap {
			db := wk.shardDBById(shardId)
			err := wk.writeMessagesBatch(db, reqs)
			if err != nil {
				return err
			}
		}
	} else {
		timeoutCtx, cancel := context.WithTimeout(wk.cancelCtx, time.Second*5)
		defer cancel()

		requestGroup, _ := errgroup.WithContext(timeoutCtx)

		for shardId, reqs := range dbMap {
			requestGroup.Go(func(sid uint32, rqs []AppendMessagesReq) func() error {
				return func() error {
					db := wk.shardDBById(sid)
					return wk.writeMessagesBatch(db, rqs)
				}
			}(shardId, reqs))

		}
		err := requestGroup.Wait()
		if err != nil {
			return err
		}
	}

	// 消息总数量增加
	err := wk.IncMessageCount(msgTotalCount)
	if err != nil {
		return err
	}

	return nil

}

func (wk *wukongDB) writeMessagesBatch(db *pebble.DB, reqs []AppendMessagesReq) error {
	batch := db.NewBatch()
	defer batch.Close()
	for _, req := range reqs {
		lastMsg := req.Messages[len(req.Messages)-1]
		for _, msg := range req.Messages {
			if err := wk.writeMessage(req.ChannelId, req.ChannelType, msg, batch); err != nil {
				return err
			}
		}
		err := wk.setChannelLastMessageSeq(req.ChannelId, req.ChannelType, uint64(lastMsg.MessageSeq), batch, wk.noSync)
		if err != nil {
			return err
		}
	}
	if err := batch.Commit(wk.sync); err != nil {
		return err
	}
	return nil
}

func (wk *wukongDB) GetMessage(messageId uint64) (Message, error) {
	messageIdKey := key.NewMessageIndexMessageIdKey(messageId)

	for _, db := range wk.dbs {
		result, closer, err := db.Get(messageIdKey)
		if err != nil {
			if err == pebble.ErrNotFound {
				continue
			}
			return EmptyMessage, err
		}
		defer closer.Close()

		if len(result) != 16 {
			return EmptyMessage, fmt.Errorf("invalid message index key")
		}
		var arr [16]byte
		copy(arr[:], result)
		iter := db.NewIter(&pebble.IterOptions{
			LowerBound: key.NewMessageColumnKeyWithPrimary(arr, key.MinColumnKey),
			UpperBound: key.NewMessageColumnKeyWithPrimary(arr, key.MaxColumnKey),
		})
		defer iter.Close()

		var msg Message
		err = wk.iteratorChannelMessages(iter, 0, func(m Message) bool {
			msg = m
			return false
		})

		if err != nil {
			return EmptyMessage, err
		}
		if IsEmptyMessage(msg) {
			return EmptyMessage, ErrNotFound
		}
		return msg, nil
	}
	return EmptyMessage, ErrNotFound
}

// 情况1: startMessageSeq=100, endMessageSeq=0, limit=10 返回的消息seq为91-100的消息 (limit生效)
// 情况2: startMessageSeq=5, endMessageSeq=0, limit=10 返回的消息seq为1-5的消息（消息无）

// 情况3: startMessageSeq=100, endMessageSeq=95, limit=10 返回的消息seq为96-100的消息（endMessageSeq生效）
// 情况4: startMessageSeq=100, endMessageSeq=50, limit=10 返回的消息seq为91-100的消息（limit生效）
func (wk *wukongDB) LoadPrevRangeMsgs(channelId string, channelType uint8, startMessageSeq, endMessageSeq uint64, limit int) ([]Message, error) {

	if startMessageSeq == 0 {
		return nil, fmt.Errorf("start messageSeq[%d] must be greater than 0", startMessageSeq)

	}
	if endMessageSeq != 0 && endMessageSeq > startMessageSeq {
		return nil, fmt.Errorf("end messageSeq[%d] must be less than start messageSeq[%d]", endMessageSeq, startMessageSeq)
	}

	var minSeq uint64
	var maxSeq uint64

	if endMessageSeq == 0 {
		maxSeq = startMessageSeq + 1
		if startMessageSeq < uint64(limit) {
			minSeq = 1
		} else {
			minSeq = startMessageSeq - uint64(limit) + 1
		}
	} else {
		maxSeq = startMessageSeq + 1
		if startMessageSeq-endMessageSeq > uint64(limit) {
			minSeq = startMessageSeq - uint64(limit) + 1
		} else {
			minSeq = endMessageSeq + 1
		}

	}

	// 获取频道的最大的messageSeq，超过这个的消息都视为无效
	lastSeq, _, err := wk.GetChannelLastMessageSeq(channelId, channelType)
	if err != nil {
		return nil, err
	}

	if maxSeq > lastSeq {
		maxSeq = lastSeq + 1
	}

	db := wk.channelDb(channelId, channelType)

	iter := db.NewIter(&pebble.IterOptions{
		LowerBound: key.NewMessagePrimaryKey(channelId, channelType, minSeq),
		UpperBound: key.NewMessagePrimaryKey(channelId, channelType, maxSeq),
	})
	defer iter.Close()

	msgs := make([]Message, 0)
	err = wk.iteratorChannelMessages(iter, limit, func(m Message) bool {
		msgs = append(msgs, m)
		return true
	})
	if err != nil {
		return nil, err
	}
	return msgs, nil
}

func (wk *wukongDB) LoadNextRangeMsgs(channelId string, channelType uint8, startMessageSeq, endMessageSeq uint64, limit int) ([]Message, error) {
	minSeq := startMessageSeq
	maxSeq := endMessageSeq
	if endMessageSeq == 0 {
		maxSeq = math.MaxUint64
	}

	// 获取频道的最大的messageSeq，超过这个的消息都视为无效
	lastSeq, _, err := wk.GetChannelLastMessageSeq(channelId, channelType)
	if err != nil {
		return nil, err
	}

	if maxSeq > lastSeq {
		maxSeq = lastSeq + 1
	}

	db := wk.channelDb(channelId, channelType)

	iter := db.NewIter(&pebble.IterOptions{
		LowerBound: key.NewMessagePrimaryKey(channelId, channelType, minSeq),
		UpperBound: key.NewMessagePrimaryKey(channelId, channelType, maxSeq),
	})
	defer iter.Close()

	msgs := make([]Message, 0)

	err = wk.iteratorChannelMessages(iter, limit, func(m Message) bool {
		msgs = append(msgs, m)
		return true
	})
	if err != nil {
		return nil, err
	}
	return msgs, nil

}

func (wk *wukongDB) LoadMsg(channelId string, channelType uint8, seq uint64) (Message, error) {

	db := wk.channelDb(channelId, channelType)

	iter := db.NewIter(&pebble.IterOptions{
		LowerBound: key.NewMessagePrimaryKey(channelId, channelType, seq),
		UpperBound: key.NewMessagePrimaryKey(channelId, channelType, seq+1),
	})
	defer iter.Close()
	var msg Message
	err := wk.iteratorChannelMessages(iter, 1, func(m Message) bool {
		msg = m
		return false
	})
	if err != nil {
		return EmptyMessage, err
	}
	if IsEmptyMessage(msg) {
		return EmptyMessage, ErrNotFound
	}
	return msg, nil

}

func (wk *wukongDB) LoadLastMsgs(channelID string, channelType uint8, limit int) ([]Message, error) {
	lastSeq, _, err := wk.GetChannelLastMessageSeq(channelID, channelType)
	if err != nil {
		return nil, err
	}
	if lastSeq == 0 {
		return nil, nil
	}
	return wk.LoadPrevRangeMsgs(channelID, channelType, lastSeq, 0, limit)

}

func (wk *wukongDB) LoadLastMsgsWithEnd(channelID string, channelType uint8, endMessageSeq uint64, limit int) ([]Message, error) {
	lastSeq, _, err := wk.GetChannelLastMessageSeq(channelID, channelType)
	if err != nil {
		return nil, err
	}
	if lastSeq == 0 {
		return nil, nil
	}
	return wk.LoadPrevRangeMsgs(channelID, channelType, lastSeq, endMessageSeq, limit)
}

func (wk *wukongDB) LoadNextRangeMsgsForSize(channelId string, channelType uint8, startMessageSeq, endMessageSeq uint64, limitSize uint64) ([]Message, error) {

	if wk.opts.EnableCost {
		start := time.Now()
		defer func() {
			cost := time.Since(start)
			if cost.Milliseconds() > 200 {
				wk.Info("loadNextRangeMsgsForSize done", zap.Duration("cost", time.Since(start)), zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Uint64("startMessageSeq", startMessageSeq), zap.Uint64("endMessageSeq", endMessageSeq))
			}
		}()
	}

	minSeq := startMessageSeq
	maxSeq := endMessageSeq

	if endMessageSeq == 0 {
		maxSeq = math.MaxUint64
	}
	db := wk.channelDb(channelId, channelType)

	iter := db.NewIter(&pebble.IterOptions{
		LowerBound: key.NewMessagePrimaryKey(channelId, channelType, minSeq),
		UpperBound: key.NewMessagePrimaryKey(channelId, channelType, maxSeq),
	})
	defer iter.Close()
	return wk.parseChannelMessagesWithLimitSize(iter, limitSize)
}

func (wk *wukongDB) TruncateLogTo(channelId string, channelType uint8, messageSeq uint64) error {
	if messageSeq == 0 {
		return fmt.Errorf("messageSeq[%d] must be greater than 0", messageSeq)

	}

	if wk.opts.EnableCost {
		start := time.Now()
		defer func() {
			wk.Info("truncateLogTo done", zap.Duration("cost", time.Since(start)), zap.String("channelId", channelId), zap.Uint8("channelType", channelType), zap.Uint64("messageSeq", messageSeq))
		}()
	}

	db := wk.channelDb(channelId, channelType)
	err := db.DeleteRange(key.NewMessagePrimaryKey(channelId, channelType, messageSeq), key.NewMessagePrimaryKey(channelId, channelType, math.MaxUint64), wk.noSync)
	if err != nil {
		return err
	}
	batch := db.NewBatch()
	defer batch.Close()

	err = batch.DeleteRange(key.NewMessagePrimaryKey(channelId, channelType, messageSeq), key.NewMessagePrimaryKey(channelId, channelType, math.MaxUint64), wk.noSync)
	if err != nil {
		return err
	}
	err = wk.setChannelLastMessageSeq(channelId, channelType, messageSeq-1, batch, wk.noSync)
	if err != nil {
		return err
	}

	return batch.Commit(wk.sync)
}

func min(x, y uint64) uint64 {
	if x < y {
		return x
	}
	return y
}
func (wk *wukongDB) GetChannelLastMessageSeq(channelId string, channelType uint8) (uint64, uint64, error) {
	db := wk.channelDb(channelId, channelType)
	result, closer, err := db.Get(key.NewChannelLastMessageSeqKey(channelId, channelType))
	if err != nil {
		if err == pebble.ErrNotFound {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	defer closer.Close()

	seq := wk.endian.Uint64(result)
	setTime := wk.endian.Uint64(result[8:])

	return seq, setTime, nil
}

func (wk *wukongDB) SetChannelLastMessageSeq(channelId string, channelType uint8, seq uint64) error {
	if wk.opts.EnableCost {
		start := time.Now()
		defer func() {
			cost := time.Since(start)
			if cost.Milliseconds() > 200 {
				wk.Info("SetChannelLastMessageSeq done", zap.Duration("cost", cost), zap.String("channelId", channelId), zap.Uint8("channelType", channelType))
			}
		}()
	}
	db := wk.channelDb(channelId, channelType)
	return wk.setChannelLastMessageSeq(channelId, channelType, seq, db, wk.sync)
}

func (wk *wukongDB) SetChannellastMessageSeqBatch(reqs []SetChannelLastMessageSeqReq) error {
	if len(reqs) == 0 {
		return nil
	}
	if wk.opts.EnableCost {
		start := time.Now()
		defer func() {
			cost := time.Since(start)
			if cost.Milliseconds() > 200 {
				wk.Info("SetChannellastMessageSeqBatch done", zap.Duration("cost", cost), zap.Int("reqs", len(reqs)))
			}
		}()
	}
	// 按照db进行分组
	dbMap := make(map[uint32][]SetChannelLastMessageSeqReq)
	for _, req := range reqs {
		shardId := wk.channelDbIndex(req.ChannelId, req.ChannelType)
		dbMap[shardId] = append(dbMap[shardId], req)
	}

	for shardId, reqs := range dbMap {
		db := wk.shardDBById(shardId)
		batch := db.NewBatch()
		defer batch.Close()
		for _, req := range reqs {
			if err := wk.setChannelLastMessageSeq(req.ChannelId, req.ChannelType, req.Seq, batch, wk.noSync); err != nil {
				return err
			}
		}
		if err := batch.Commit(wk.sync); err != nil {
			return err
		}
	}
	return nil
}

var minMessagePrimaryKey = [16]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
var maxMessagePrimaryKey = [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func (wk *wukongDB) searchMessageByIndex(req MessageSearchReq, db *pebble.DB, iterFnc func(m Message) bool) (bool, error) {
	var lowKey []byte
	var highKey []byte

	var existKey = false

	if strings.TrimSpace(req.FromUid) != "" {

		lowKey = key.NewMessageSecondIndexFromUidKey(req.FromUid, minMessagePrimaryKey)
		highKey = key.NewMessageSecondIndexFromUidKey(req.FromUid, maxMessagePrimaryKey)
		existKey = true
	}

	// if req.MessageId > 0 && !existKey {
	// 	lowKey = key.NewMessageIndexMessageIdKey(uint64(req.MessageId), minMessagePrimaryKey)
	// 	highKey = key.NewMessageIndexMessageIdKey(uint64(req.MessageId), maxMessagePrimaryKey)
	// 	existKey = true
	// }

	if strings.TrimSpace(req.ClientMsgNo) != "" && !existKey {
		lowKey = key.NewMessageSecondIndexClientMsgNoKey(req.ClientMsgNo, minMessagePrimaryKey)
		highKey = key.NewMessageSecondIndexClientMsgNoKey(req.ClientMsgNo, maxMessagePrimaryKey)
		existKey = true
	}

	if !existKey {
		return false, nil
	}

	iter := db.NewIter(&pebble.IterOptions{
		LowerBound: lowKey,
		UpperBound: highKey,
	})
	defer iter.Close()

	for iter.Last(); iter.Valid(); iter.Prev() {
		primaryBytes, err := key.ParseMessageSecondIndexKey(iter.Key())
		if err != nil {
			wk.Error("parseMessageIndexKey", zap.Error(err))
			continue
		}

		iter := db.NewIter(&pebble.IterOptions{
			LowerBound: key.NewMessageColumnKeyWithPrimary(primaryBytes, key.MinColumnKey),
			UpperBound: key.NewMessageColumnKeyWithPrimary(primaryBytes, key.MaxColumnKey),
		})

		defer iter.Close()

		var msg Message
		err = wk.iteratorChannelMessages(iter, 0, func(m Message) bool {
			msg = m
			return false
		})
		if err != nil {
			return false, err
		}
		if iterFnc != nil {
			if !iterFnc(msg) {
				break
			}
		}
	}

	return true, nil

}

func (wk *wukongDB) SearchMessages(req MessageSearchReq) ([]Message, error) {

	if req.MessageId > 0 { // 如果指定了messageId，则直接查询messageId，这种情况要么没有要么只有一条
		msg, err := wk.GetMessage(uint64(req.MessageId))
		if err != nil {
			if err == ErrNotFound {
				return nil, nil
			}
			return nil, err
		}
		return []Message{msg}, nil
	}

	currentSize := 0
	msgs := make([]Message, 0)
	iterFnc := func(m Message) bool {
		if strings.TrimSpace(req.ChannelId) != "" && m.ChannelID != req.ChannelId {
			return true
		}

		if req.ChannelType != 0 && req.ChannelType != m.ChannelType {
			return true
		}

		if strings.TrimSpace(req.FromUid) != "" && m.FromUID != req.FromUid {
			return true
		}

		if len(req.Payload) > 0 && !bytes.Contains(m.Payload, req.Payload) {
			return true
		}

		if req.MessageId > 0 && req.MessageId != m.MessageID {
			return true
		}

		if currentSize > req.Limit*req.CurrentPage { // 大于当前页的消息终止遍历
			return false
		}

		currentSize++

		if currentSize > (req.CurrentPage-1)*req.Limit && currentSize <= req.CurrentPage*req.Limit {
			msgs = append(msgs, m)
			return true
		}
		return true
	}
	for _, db := range wk.dbs {

		// 通过索引查询
		has, err := wk.searchMessageByIndex(req, db, iterFnc)
		if err != nil {
			return nil, err
		}

		if has { // 如果有触发索引，则无需全局查询
			continue
		}

		iter := db.NewIter(&pebble.IterOptions{
			LowerBound: key.NewMessageSearchLowKeWith(req.ChannelId, req.ChannelType, 0),
			UpperBound: key.NewMessageSearchHighKeWith(req.ChannelId, req.ChannelType, math.MaxUint64),
		})
		defer iter.Close()

		err = wk.iteratorChannelMessagesDirection(iter, 0, true, iterFnc)
		if err != nil {
			return nil, err
		}
	}

	if req.Limit > 0 && len(msgs) > req.Limit {
		return msgs[:req.Limit], nil
	}

	return msgs, nil
}

func (wk *wukongDB) setChannelLastMessageSeq(channelId string, channelType uint8, seq uint64, w pebble.Writer, o *pebble.WriteOptions) error {
	data := make([]byte, 16)
	wk.endian.PutUint64(data, seq)
	setTime := time.Now().UnixNano()
	wk.endian.PutUint64(data[8:], uint64(setTime))

	return w.Set(key.NewChannelLastMessageSeqKey(channelId, channelType), data, o)
}

func (wk *wukongDB) iteratorChannelMessages(iter *pebble.Iterator, limit int, iterFnc func(m Message) bool) error {
	return wk.iteratorChannelMessagesDirection(iter, limit, false, iterFnc)
}

func (wk *wukongDB) iteratorChannelMessagesDirection(iter *pebble.Iterator, limit int, reverse bool, iterFnc func(m Message) bool) error {
	var (
		size           int
		preMessageSeq  uint64
		preMessage     Message
		lastNeedAppend bool = true
		hasData        bool = false
	)

	if reverse {
		if !iter.Last() {
			return nil
		}
	} else {
		if !iter.First() {
			return nil
		}
	}
	for iter.Valid() {
		if reverse {
			if !iter.Prev() {
				break
			}
		} else {
			if !iter.Next() {
				break
			}
		}
		messageSeq, coulmnName, err := key.ParseMessageColumnKey(iter.Key())
		if err != nil {
			return err
		}

		if preMessageSeq != messageSeq {
			if preMessageSeq != 0 {
				size++
				if iterFnc != nil {
					if !iterFnc(preMessage) {
						lastNeedAppend = false
						break
					}
				}
				if limit != 0 && size >= limit {
					lastNeedAppend = false
					break
				}
			}

			preMessageSeq = messageSeq
			preMessage = Message{}
			preMessage.MessageSeq = uint32(messageSeq)
		}

		switch coulmnName {
		case key.TableMessage.Column.Header:
			preMessage.Framer = wkproto.FramerFromUint8(iter.Value()[0])
		case key.TableMessage.Column.Setting:
			preMessage.Setting = wkproto.Setting(iter.Value()[0])
		case key.TableMessage.Column.Expire:
			preMessage.Expire = wk.endian.Uint32(iter.Value())
		case key.TableMessage.Column.MessageId:
			preMessage.MessageID = int64(wk.endian.Uint64(iter.Value()))
		case key.TableMessage.Column.ClientMsgNo:
			preMessage.ClientMsgNo = string(iter.Value())
		case key.TableMessage.Column.Timestamp:
			preMessage.Timestamp = int32(wk.endian.Uint32(iter.Value()))
		case key.TableMessage.Column.ChannelId:
			preMessage.ChannelID = string(iter.Value())
		case key.TableMessage.Column.ChannelType:
			preMessage.ChannelType = iter.Value()[0]
		case key.TableMessage.Column.Topic:
			preMessage.Topic = string(iter.Value())
		case key.TableMessage.Column.FromUid:
			preMessage.FromUID = string(iter.Value())
		case key.TableMessage.Column.Payload:
			// 这里必须复制一份，否则会被pebble覆盖
			var payload = make([]byte, len(iter.Value()))
			copy(payload, iter.Value())
			preMessage.Payload = payload
		case key.TableMessage.Column.Term:
			preMessage.Term = wk.endian.Uint64(iter.Value())

		}
		hasData = true
	}
	if lastNeedAppend && hasData {
		if iterFnc != nil {

			_ = iterFnc(preMessage)
		}
	}

	return nil

}

func (wk *wukongDB) parseChannelMessagesWithLimitSize(iter *pebble.Iterator, limitSize uint64) ([]Message, error) {
	var (
		msgs           = make([]Message, 0)
		preMessageSeq  uint64
		preMessage     Message
		lastNeedAppend bool = false
	)

	var size uint64 = 0
	for iter.First(); iter.Valid(); iter.Next() {
		lastNeedAppend = true
		messageSeq, coulmnName, err := key.ParseMessageColumnKey(iter.Key())
		if err != nil {
			return nil, err
		}

		if messageSeq == 0 {
			wk.Panic("messageSeq is 0", zap.Any("key", iter.Key()), zap.Any("coulmnName", coulmnName))
		}

		if preMessageSeq != messageSeq {
			if preMessageSeq != 0 {
				size += uint64(preMessage.Size())
				msgs = append(msgs, preMessage)
				if limitSize != 0 && size >= limitSize {
					lastNeedAppend = false
					break
				}
			}

			preMessageSeq = messageSeq
			preMessage = Message{}
			preMessage.MessageSeq = uint32(messageSeq)
		}

		switch coulmnName {
		case key.TableMessage.Column.Header:
			preMessage.RecvPacket.Framer = wkproto.FramerFromUint8(iter.Value()[0])
		case key.TableMessage.Column.Setting:
			preMessage.RecvPacket.Setting = wkproto.Setting(iter.Value()[0])
		case key.TableMessage.Column.Expire:
			preMessage.RecvPacket.Expire = wk.endian.Uint32(iter.Value())
		case key.TableMessage.Column.MessageId:
			preMessage.MessageID = int64(wk.endian.Uint64(iter.Value()))
		case key.TableMessage.Column.ClientMsgNo:
			preMessage.ClientMsgNo = string(iter.Value())
		case key.TableMessage.Column.Timestamp:
			preMessage.Timestamp = int32(wk.endian.Uint32(iter.Value()))
		case key.TableMessage.Column.ChannelId:
			preMessage.ChannelID = string(iter.Value())
		case key.TableMessage.Column.ChannelType:
			preMessage.ChannelType = iter.Value()[0]
		case key.TableMessage.Column.Topic:
			preMessage.Topic = string(iter.Value())
		case key.TableMessage.Column.FromUid:
			preMessage.RecvPacket.FromUID = string(iter.Value())
		case key.TableMessage.Column.Payload:
			// 这里必须复制一份，否则会被pebble覆盖
			var payload = make([]byte, len(iter.Value()))
			copy(payload, iter.Value())
			preMessage.Payload = payload
		case key.TableMessage.Column.Term:
			preMessage.Term = wk.endian.Uint64(iter.Value())
		}
	}

	if lastNeedAppend {
		msgs = append(msgs, preMessage)
	}

	return msgs, nil

}

func (wk *wukongDB) writeMessage(channelId string, channelType uint8, msg Message, w pebble.Writer) error {

	var (
		messageIdBytes = make([]byte, 8)
		err            error
	)

	// header
	header := wkproto.ToFixHeaderUint8(msg.RecvPacket.Framer)
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Header), []byte{header}, wk.noSync); err != nil {
		return err
	}

	// setting
	setting := msg.RecvPacket.Setting.Uint8()
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Setting), []byte{setting}, wk.noSync); err != nil {
		return err
	}

	// expire
	expireBytes := make([]byte, 4)
	wk.endian.PutUint32(expireBytes, msg.RecvPacket.Expire)
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Expire), expireBytes, wk.noSync); err != nil {
		return err
	}

	// messageId
	wk.endian.PutUint64(messageIdBytes, uint64(msg.MessageID))
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.MessageId), messageIdBytes, wk.noSync); err != nil {
		return err
	}

	// messageSeq
	messageSeqBytes := make([]byte, 8)
	wk.endian.PutUint64(messageSeqBytes, uint64(msg.MessageSeq))
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.MessageSeq), messageSeqBytes, wk.noSync); err != nil {
		return err
	}

	// clientMsgNo
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.ClientMsgNo), []byte(msg.ClientMsgNo), wk.noSync); err != nil {
		return err
	}

	// timestamp
	timestampBytes := make([]byte, 4)
	wk.endian.PutUint32(timestampBytes, uint32(msg.Timestamp))
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Timestamp), timestampBytes, wk.noSync); err != nil {
		return err
	}

	// channelId
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.ChannelId), []byte(msg.ChannelID), wk.noSync); err != nil {
		return err
	}

	// channelType
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.ChannelType), []byte{msg.ChannelType}, wk.noSync); err != nil {
		return err
	}

	// topic
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Topic), []byte(msg.Topic), wk.noSync); err != nil {
		return err
	}

	// fromUid
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.FromUid), []byte(msg.RecvPacket.FromUID), wk.noSync); err != nil {
		return err
	}

	// payload
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Payload), msg.Payload, wk.noSync); err != nil {
		return err
	}

	// term
	termBytes := make([]byte, 8)
	wk.endian.PutUint64(termBytes, msg.Term)
	if err = w.Set(key.NewMessageColumnKey(channelId, channelType, uint64(msg.MessageSeq), key.TableMessage.Column.Term), termBytes, wk.noSync); err != nil {
		return err
	}

	var primaryValue = [16]byte{}
	wk.endian.PutUint64(primaryValue[:], key.ChannelIdToNum(channelId, channelType))
	wk.endian.PutUint64(primaryValue[8:], uint64(msg.MessageSeq))

	// index fromUid
	if err = w.Set(key.NewMessageSecondIndexFromUidKey(msg.FromUID, primaryValue), nil, wk.noSync); err != nil {
		return err
	}

	// index messageId
	if err = w.Set(key.NewMessageIndexMessageIdKey(uint64(msg.MessageID)), primaryValue[:], wk.noSync); err != nil {
		return err
	}

	// index clientMsgNo
	if err = w.Set(key.NewMessageSecondIndexClientMsgNoKey(msg.ClientMsgNo, primaryValue), nil, wk.noSync); err != nil {
		return err
	}

	// index timestamp
	if err = w.Set(key.NewMessageIndexTimestampKey(uint64(msg.Timestamp), primaryValue), nil, wk.noSync); err != nil {
		return err
	}

	return nil
}