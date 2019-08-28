// Package oplog tails a MongoDB oplog, process each message, and generates
// the message that should be sent to Redis. It writes these to an output
// channel that should be read by package redispub and sent to the Redis server.
package oplog

import (
	"strings"
	"time"

	"github.com/tulip/oplogtoredis/lib/log"
	"github.com/tulip/oplogtoredis/lib/redispub"

	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/go-redis/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Tailer persistently tails the oplog of a Mongo cluster, handling
// reconnection and resumption of where it left off.
type Tailer struct {
	MongoClient *mgo.Session
	RedisClient redis.UniversalClient
	RedisPrefix string
	MaxCatchUp  time.Duration
}

// Raw oplog entry from Mongo
type rawOplogEntry struct {
	Timestamp    bson.MongoTimestamp `bson:"ts"`
	HistoryID    int64               `bson:"h"`
	MongoVersion int                 `bson:"v"`
	Operation    string              `bson:"op"`
	Namespace    string              `bson:"ns"`
	Doc          bson.Raw            `bson:"o"`
	Update       rawOplogEntryID     `bson:"o2"`
}

type rawOplogEntryID struct {
	ID interface{} `bson:"_id"`
}

const requeryDuration = time.Second

var (
	metricOplogEntriesReceived = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "otr",
		Subsystem: "oplog",
		Name:      "entries_by_size",
		Help:      "Oplog entries by size.",
	}, []string{"database", "status"})
)

// Tail begins tailing the oplog. It doesn't return unless it receives a message
// on the stop channel, in which case it wraps up its work and then returns.
func (tailer *Tailer) Tail(out chan<- *redispub.Publication, stop <-chan bool) {
	childStopC := make(chan bool)
	wasStopped := false

	go func() {
		<-stop
		wasStopped = true
		childStopC <- true
	}()

	for {
		log.Log.Info("Starting oplog tailing")
		tailer.tailOnce(out, childStopC)
		log.Log.Info("Oplog tailing ended")

		if wasStopped {
			return
		}

		log.Log.Errorw("Oplog tailing stopped prematurely. Waiting a second an then retrying.")
		time.Sleep(requeryDuration)
	}
}

func (tailer *Tailer) tailOnce(out chan<- *redispub.Publication, stop <-chan bool) {
	session := tailer.MongoClient.Copy()
	oplogCollection := session.DB("local").C("oplog.rs")

	startTime := tailer.getStartTime(func() (bson.MongoTimestamp, error) {
		// Get the timestamp of the last entry in the oplog (as a position to
		// start from if we don't have a last-written timestamp from Redis)
		var entry rawOplogEntry
		mongoErr := oplogCollection.Find(bson.M{}).Sort("-$natural").One(&entry)

		log.Log.Infow("Got latest oplog entry",
			"entry", entry,
			"error", mongoErr)

		return entry.Timestamp, mongoErr
	})

	query := oplogCollection.Find(bson.M{"ts": bson.M{"$gt": startTime}})
	iter := query.LogReplay().Sort("$natural").Tail(requeryDuration)

	var lastTimestamp bson.MongoTimestamp
	for {
		select {
		case <-stop:
			log.Log.Infof("Received stop; aborting oplog tailing")
			return
		default:
		}

		var rawData bson.Raw

		for iter.Next(&rawData) {
			ts, pubs := tailer.unmarshalEntry(rawData)

			if ts != nil {
				lastTimestamp = *ts
			}

			for _, pub := range pubs {
				out <- pub
			}
		}

		if iter.Err() != nil {
			log.Log.Errorw("Error from oplog iterator",
				"error", iter.Err())

			closeErr := iter.Close()
			if closeErr != nil {
				log.Log.Errorw("Error from closing oplog iterator",
					"error", closeErr)
			}

			return
		}

		if iter.Timeout() {
			// Didn't get any messages for a while, keep trying
			log.Log.Info("Oplog cursor timed out, will retry")
			continue
		}

		// Our cursor expired. Make a new cursor to pick up from where we
		// left off.
		query := oplogCollection.Find(bson.M{"ts": bson.M{"$gt": lastTimestamp}})
		iter = query.LogReplay().Sort("$natural").Tail(requeryDuration)
	}
}

// unmarshalEntry unmarshals a single entry from the oplog.
//
// The timestamp of the entry is returned so that tailOnce knows the timestamp of the last entry it read, even if it
// ignored it or failed at some later step.
func (tailer *Tailer) unmarshalEntry(rawData bson.Raw) (timestamp *bson.MongoTimestamp, pubs []*redispub.Publication) {
	var result rawOplogEntry

	err := rawData.Unmarshal(&result)
	if err != nil {
		log.Log.Errorw("Error unmarshaling oplog entry", "error", err)
		return
	}

	timestamp = &result.Timestamp

	entries := tailer.parseRawOplogEntry(result, nil)
	log.Log.Debugw("Received oplog entry",
		"entry", result)

	status := "ignored"
	database := "(no database)"
	defer func() {
		metricOplogEntriesReceived.WithLabelValues(database, status).Observe(float64(len(rawData.Data)))
	}()

	if len(entries) == 0 {
		return
	}

	database = entries[0].Database

	for _, entry := range entries {
		pub, err := processOplogEntry(&entry)
		if err != nil {
			status = "error"
			pub = nil

			log.Log.Errorw("Error processing oplog entry",
				"op", entry,
				"error", err,
				"database", entry.Database,
				"collection", entry.Collection)
		} else {
			status = "processed"
			pubs = append(pubs, pub)
		}
	}

	return
}

// Gets the bson.MongoTimestamp from which we should start tailing
//
// We take the function to get the timestamp of the last oplog entry (as a
// fallback if we don't have a latest timestamp from Redis) as an arg instead
// of using tailer.mongoClient directly so we can unit test this function
func (tailer *Tailer) getStartTime(getTimestampOfLastOplogEntry func() (bson.MongoTimestamp, error)) bson.MongoTimestamp {
	ts, tsTime, redisErr := redispub.LastProcessedTimestamp(tailer.RedisClient, tailer.RedisPrefix)

	if redisErr == nil {
		// we have a last write time, check that it's not too far in the
		// past
		if tsTime.After(time.Now().Add(-1 * tailer.MaxCatchUp)) {
			log.Log.Infof("Found last processed timestamp, resuming oplog tailing from %d", tsTime.Unix())
			return ts
		}

		log.Log.Warnf("Found last processed timestamp, but it was too far in the past (%d). Will start from end of oplog", tsTime.Unix())
	}

	if (redisErr != nil) && (redisErr != redis.Nil) {
		log.Log.Errorw("Error querying Redis for last processed timestamp. Will start from end of oplog.",
			"error", redisErr)
	}

	mongoOplogEndTimestamp, mongoErr := getTimestampOfLastOplogEntry()
	if mongoErr == nil {
		log.Log.Infof("Starting tailing from end of oplog (timestamp %d)", int64(mongoOplogEndTimestamp))
		return mongoOplogEndTimestamp
	}

	log.Log.Errorw("Got error when asking for last operation timestamp in the oplog. Returning current time.",
		"error", mongoErr)
	return bson.MongoTimestamp(time.Now().Unix() << 32)
}

// converts a rawOplogEntry to an oplogEntry
func (tailer *Tailer) parseRawOplogEntry(entry rawOplogEntry, txIdx *uint) []oplogEntry {
	if txIdx == nil {
		idx := uint(0)
		txIdx = &idx
	}

	switch entry.Operation {
	case operationInsert, operationUpdate, operationRemove:
		var data map[string]interface{}
		if err := entry.Doc.Unmarshal(&data); err != nil {
			log.Log.Errorf("unmarshalling oplog entry data: %v", err)
			return nil
		}

		out := oplogEntry{
			Operation: entry.Operation,
			Timestamp: entry.Timestamp,
			Namespace: entry.Namespace,
			Data:      data,

			TxIdx: *txIdx,
		}

		*txIdx++

		out.Database, out.Collection = parseNamespace(out.Namespace)

		if out.Operation == operationUpdate {
			out.DocID = entry.Update.ID
		} else {
			out.DocID = data["_id"]
		}

		return []oplogEntry{out}

	case operationCommand:
		if entry.Namespace != "admin.$cmd" {
			return nil
		}

		var txData struct {
			ApplyOps []rawOplogEntry `bson:"applyOps"`
		}

		if err := entry.Doc.Unmarshal(&txData); err != nil {
			log.Log.Errorf("unmarshaling transaction data: %v", err)
			return nil
		}

		var ret []oplogEntry

		for _, v := range txData.ApplyOps {
			v.Timestamp = entry.Timestamp
			ret = append(ret, tailer.parseRawOplogEntry(v, txIdx)...)
		}

		return ret

	default:
		return nil
	}
}

// Parses op.Namespace into (database, collection)
func parseNamespace(namespace string) (string, string) {
	namespaceParts := strings.SplitN(namespace, ".", 2)

	database := namespaceParts[0]
	collection := ""
	if len(namespaceParts) > 1 {
		collection = namespaceParts[1]
	}

	return database, collection
}
