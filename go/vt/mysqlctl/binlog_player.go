// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mysqlctl

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/mysql"
	mproto "github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/rpcplus"
	"github.com/youtube/vitess/go/stats"
	"github.com/youtube/vitess/go/vt/key"
	"github.com/youtube/vitess/go/vt/mysqlctl/proto"
)

var (
	SLOW_QUERY_THRESHOLD      = time.Duration(100 * time.Millisecond)
	BLPL_STREAM_COMMENT_START = []byte("/* _stream ")
	BLPL_SPACE                = []byte(" ")
)

// VtClient is a high level interface to the database
type VtClient interface {
	Connect() error
	Begin() error
	Commit() error
	Rollback() error
	Close()
	ExecuteFetch(query string, maxrows int, wantfields bool) (qr *mproto.QueryResult, err error)
}

// DummyVtClient is a VtClient that writes to a writer instead of executing
// anything
type DummyVtClient struct {
	stdout *bufio.Writer
}

func NewDummyVtClient() *DummyVtClient {
	stdout := bufio.NewWriterSize(os.Stdout, 16*1024)
	return &DummyVtClient{stdout}
}

func (dc DummyVtClient) Connect() error {
	return nil
}

func (dc DummyVtClient) Begin() error {
	dc.stdout.WriteString("BEGIN;\n")
	return nil
}
func (dc DummyVtClient) Commit() error {
	dc.stdout.WriteString("COMMIT;\n")
	return nil
}
func (dc DummyVtClient) Rollback() error {
	dc.stdout.WriteString("ROLLBACK;\n")
	return nil
}
func (dc DummyVtClient) Close() {
	return
}

func (dc DummyVtClient) ExecuteFetch(query string, maxrows int, wantfields bool) (qr *mproto.QueryResult, err error) {
	dc.stdout.WriteString(string(query) + ";\n")
	return &mproto.QueryResult{Fields: nil, RowsAffected: 1, InsertId: 0, Rows: nil}, nil
}

// DBClient is a real VtClient backed by a mysql connection
type DBClient struct {
	dbConfig *mysql.ConnectionParams
	dbConn   *mysql.Connection
}

func NewDbClient(dbConfig *mysql.ConnectionParams) *DBClient {
	dbClient := &DBClient{}
	dbClient.dbConfig = dbConfig
	return dbClient
}

func (dc *DBClient) handleError(err error) {
	// log.Errorf("in DBClient handleError %v", err.(error))
	if sqlErr, ok := err.(*mysql.SqlError); ok {
		if sqlErr.Number() >= 2000 && sqlErr.Number() <= 2018 { // mysql connection errors
			dc.Close()
		}
		if sqlErr.Number() == 1317 { // Query was interrupted
			dc.Close()
		}
	}
}

func (dc *DBClient) Connect() error {
	var err error
	dc.dbConn, err = mysql.Connect(*dc.dbConfig)
	if err != nil {
		return fmt.Errorf("error in connecting to mysql db, err %v", err)
	}
	return nil
}

func (dc *DBClient) Begin() error {
	_, err := dc.dbConn.ExecuteFetch("begin", 1, false)
	if err != nil {
		log.Errorf("BEGIN failed w/ error %v", err)
		dc.handleError(err)
	}
	return err
}

func (dc *DBClient) Commit() error {
	_, err := dc.dbConn.ExecuteFetch("commit", 1, false)
	if err != nil {
		log.Errorf("COMMIT failed w/ error %v", err)
		dc.dbConn.Close()
	}
	return err
}

func (dc *DBClient) Rollback() error {
	_, err := dc.dbConn.ExecuteFetch("rollback", 1, false)
	if err != nil {
		log.Errorf("ROLLBACK failed w/ error %v", err)
		dc.dbConn.Close()
	}
	return err
}

func (dc *DBClient) Close() {
	if dc.dbConn != nil {
		dc.dbConn.Close()
		dc.dbConn = nil
	}
}

func (dc *DBClient) ExecuteFetch(query string, maxrows int, wantfields bool) (*mproto.QueryResult, error) {
	mqr, err := dc.dbConn.ExecuteFetch(query, maxrows, wantfields)
	if err != nil {
		log.Errorf("ExecuteFetch failed w/ error %v", err)
		dc.handleError(err)
		return nil, err
	}
	qr := mproto.QueryResult(*mqr)
	return &qr, nil
}

// blplStats is the internal stats of this player
type blplStats struct {
	queryCount    *stats.Counters
	txnCount      *stats.Counters
	queriesPerSec *stats.Rates
	txnsPerSec    *stats.Rates
	txnTime       *stats.Timings
	queryTime     *stats.Timings
}

func NewBlplStats() *blplStats {
	bs := &blplStats{}
	bs.txnCount = stats.NewCounters("")
	bs.queryCount = stats.NewCounters("")
	bs.queriesPerSec = stats.NewRates("", bs.queryCount, 15, 60e9)
	bs.txnsPerSec = stats.NewRates("", bs.txnCount, 15, 60e9)
	bs.txnTime = stats.NewTimings("")
	bs.queryTime = stats.NewTimings("")
	return bs
}

// statsJSON returns a json encoded version of stats
func (bs *blplStats) statsJSON() string {
	buf := bytes.NewBuffer(make([]byte, 0, 128))
	fmt.Fprintf(buf, "{")
	fmt.Fprintf(buf, "\n \"TxnCount\": %v,", bs.txnCount)
	fmt.Fprintf(buf, "\n \"QueryCount\": %v,", bs.queryCount)
	fmt.Fprintf(buf, "\n \"QueriesPerSec\": %v,", bs.queriesPerSec)
	fmt.Fprintf(buf, "\n \"TxnPerSec\": %v", bs.txnsPerSec)
	fmt.Fprintf(buf, "\n \"TxnTime\": %v,", bs.txnTime)
	fmt.Fprintf(buf, "\n \"QueryTime\": %v,", bs.queryTime)
	fmt.Fprintf(buf, "\n}")
	return buf.String()
}

// BinlogPlayer is handling reading a stream of updates from BinlogServer
type BinlogPlayer struct {
	addr          string
	dbClient      VtClient
	keyRange      key.KeyRange
	tables        []string
	blpPos        proto.BlpPosition
	stopAtGroupId int64
	blplStats     *blplStats
}

// NewBinlogPlayerKeyRange returns a new BinlogPlayer pointing at the server
// replicating the provided keyrange, starting at the startPosition.GroupId,
// and updating _vt.blp_checkpoint with uid=startPosition.Uid.
// If stopAtGroupId != 0, it will stop when reaching that GroupId.
func NewBinlogPlayerKeyRange(dbClient VtClient, addr string, keyRange key.KeyRange, startPosition *proto.BlpPosition, stopAtGroupId int64) *BinlogPlayer {
	return &BinlogPlayer{
		addr:          addr,
		dbClient:      dbClient,
		keyRange:      keyRange,
		blpPos:        *startPosition,
		stopAtGroupId: stopAtGroupId,
		blplStats:     NewBlplStats(),
	}
}

// NewBinlogPlayerTables returns a new BinlogPlayer pointing at the server
// replicating the provided tables, starting at the startPosition.GroupId,
// and updating _vt.blp_checkpoint with uid=startPosition.Uid.
// If stopAtGroupId != 0, it will stop when reaching that GroupId.
func NewBinlogPlayerTables(dbClient VtClient, addr string, tables []string, startPosition *proto.BlpPosition, stopAtGroupId int64) *BinlogPlayer {
	return &BinlogPlayer{
		addr:          addr,
		dbClient:      dbClient,
		tables:        tables,
		blpPos:        *startPosition,
		stopAtGroupId: stopAtGroupId,
		blplStats:     NewBlplStats(),
	}
}

func (blp *BinlogPlayer) StatsJSON() string {
	return blp.blplStats.statsJSON()
}

func (blp *BinlogPlayer) writeRecoveryPosition(groupId int64) error {
	blp.blpPos.GroupId = groupId
	updateRecovery := fmt.Sprintf(
		"update _vt.blp_checkpoint set group_id=%v, time_updated=%v where source_shard_uid=%v",
		groupId,
		time.Now().Unix(),
		blp.blpPos.Uid)

	qr, err := blp.exec(updateRecovery)
	if err != nil {
		return fmt.Errorf("Error %v in writing recovery info %v", err, updateRecovery)
	}
	if qr.RowsAffected != 1 {
		return fmt.Errorf("Cannot update blp_recovery table, affected %v rows", qr.RowsAffected)
	}
	return nil
}

func ReadStartPosition(dbClient VtClient, uid uint32) (*proto.BlpPosition, error) {
	selectRecovery := fmt.Sprintf(
		"select group_id from _vt.blp_checkpoint where source_shard_uid=%v",
		uid)
	qr, err := dbClient.ExecuteFetch(selectRecovery, 1, true)
	if err != nil {
		return nil, fmt.Errorf("error %v in selecting from recovery table %v", err, selectRecovery)
	}
	if qr.RowsAffected != 1 {
		return nil, fmt.Errorf("checkpoint information not available in db for %v", uid)
	}
	temp, err := qr.Rows[0][0].ParseInt64()
	if err != nil {
		return nil, err
	}
	return &proto.BlpPosition{
		Uid:     uid,
		GroupId: temp,
	}, nil
}

func (blp *BinlogPlayer) processTransaction(tx *proto.BinlogTransaction) (ok bool, err error) {
	txnStartTime := time.Now()
	if err = blp.dbClient.Begin(); err != nil {
		return false, fmt.Errorf("failed query BEGIN, err: %s", err)
	}
	if err = blp.writeRecoveryPosition(tx.GroupId); err != nil {
		return false, err
	}
	for _, stmt := range tx.Statements {
		if _, err = blp.exec(string(stmt.Sql)); err == nil {
			continue
		}
		if sqlErr, ok := err.(*mysql.SqlError); ok && sqlErr.Number() == 1213 {
			// Deadlock: ask for retry
			log.Infof("Deadlock: %v", err)
			if err = blp.dbClient.Rollback(); err != nil {
				return false, err
			}
			return false, nil
		}
		return false, err
	}
	if err = blp.dbClient.Commit(); err != nil {
		return false, fmt.Errorf("failed query COMMIT, err: %s", err)
	}
	blp.blplStats.txnCount.Add("TxnCount", 1)
	blp.blplStats.txnTime.Record("TxnTime", txnStartTime)
	return true, nil
}

func (blp *BinlogPlayer) exec(sql string) (*mproto.QueryResult, error) {
	queryStartTime := time.Now()
	qr, err := blp.dbClient.ExecuteFetch(sql, 0, false)
	blp.blplStats.queryCount.Add("QueryCount", 1)
	blp.blplStats.queryTime.Record("QueryTime", queryStartTime)
	if time.Now().Sub(queryStartTime) > SLOW_QUERY_THRESHOLD {
		log.Infof("SLOW QUERY '%s'", sql)
	}
	return qr, err
}

// ApplyBinlogEvents makes a gob rpc request to BinlogServer
// and processes the events. It will return nil if 'interrupted'
// was closed, or if we reached the stopping point.
// It will return io.EOF if the server stops sending us updates.
// It may return any other error it encounters.
func (blp *BinlogPlayer) ApplyBinlogEvents(interrupted chan struct{}) error {
	if len(blp.tables) > 0 {
		log.Infof("BinlogPlayer client %v for tables %v starting @ '%v', server: %v",
			blp.blpPos.Uid,
			blp.tables,
			blp.blpPos.GroupId,
			blp.addr,
		)
	} else {
		log.Infof("BinlogPlayer client %v for keyrange '%v-%v' starting @ '%v', server: %v",
			blp.blpPos.Uid,
			blp.keyRange.Start.Hex(),
			blp.keyRange.End.Hex(),
			blp.blpPos.GroupId,
			blp.addr,
		)
	}
	if blp.stopAtGroupId > 0 {
		// we need to stop at some point
		// sanity check the point
		if blp.blpPos.GroupId > blp.stopAtGroupId {
			return fmt.Errorf("starting point %v greater than stopping point %v", blp.blpPos.GroupId, blp.stopAtGroupId)
		} else if blp.blpPos.GroupId == blp.stopAtGroupId {
			log.Infof("Not starting BinlogPlayer, we're already at the desired position %v", blp.stopAtGroupId)
			return nil
		}
		log.Infof("Will stop player when reaching %v", blp.stopAtGroupId)
	}
	rpcClient, err := rpcplus.DialHTTP("tcp", blp.addr)
	defer rpcClient.Close()
	if err != nil {
		log.Errorf("Error dialing binlog server: %v", err)
		return fmt.Errorf("error dialing binlog server: %v", err)
	}

	responseChan := make(chan *proto.BinlogTransaction)
	var resp *rpcplus.Call
	if len(blp.tables) > 0 {
		req := &proto.TablesRequest{
			Tables:  blp.tables,
			GroupId: blp.blpPos.GroupId,
		}
		resp = rpcClient.StreamGo("UpdateStream.StreamTables", req, responseChan)
	} else {
		req := &proto.KeyRangeRequest{
			KeyRange: blp.keyRange,
			GroupId:  blp.blpPos.GroupId,
		}
		resp = rpcClient.StreamGo("UpdateStream.StreamKeyRange", req, responseChan)
	}

processLoop:
	for {
		select {
		case response, ok := <-responseChan:
			if !ok {
				break processLoop
			}
			for {
				ok, err = blp.processTransaction(response)
				if err != nil {
					return fmt.Errorf("Error in processing binlog event %v", err)
				}
				if ok {
					if blp.stopAtGroupId > 0 && blp.blpPos.GroupId >= blp.stopAtGroupId {
						log.Infof("Reached stopping position, done playing logs")
						return nil
					}
					break
				}
				log.Infof("Retrying txn")
				time.Sleep(1 * time.Second)
			}
		case <-interrupted:
			return nil
		}
	}
	if resp.Error != nil {
		return fmt.Errorf("Error received from ServeBinlog %v", resp.Error)
	}
	return io.EOF
}
