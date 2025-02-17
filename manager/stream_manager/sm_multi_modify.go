package stream_manager

import (
	"bytes"
	"context"
	"fmt"

	"github.com/journeymidnight/autumn/etcd_utils"
	"github.com/journeymidnight/autumn/proto/pb"
	"github.com/journeymidnight/autumn/proto/pspb"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/wire_errors"
	"github.com/pkg/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

//duplicateStream copy all extents in srcStream to desStream.
func (sm *StreamManager) duplicateStream(ops *[]clientv3.Op, srcStreamID uint64, destStreamID uint64, sealedLength uint32) (func(), func(), error) {

	streamInfo, ok  := sm.cloneStreamInfo(srcStreamID)
	if !ok {
		return nil, nil, errors.Errorf("stream %d no exist", srcStreamID)
	}
	newStreamInfo := pb.StreamInfo{
		StreamID: destStreamID,
	}

	unlockExtents := func () {
		for _, extentID := range streamInfo.ExtentIDs {
			sm.unlockExtent(extentID)
		}
	}
	


	var updateExInfos []*pb.ExtentInfo

	//unlock

	
	for i, extentID := range streamInfo.ExtentIDs {
		if err := sm.lockExtent(extentID); err != nil {
			for j := 0; j < i; j++ {
				sm.unlockExtent(streamInfo.ExtentIDs[j])
			}
			return nil, nil, err
		}
		exInfo, ok := sm.cloneExtentInfo(extentID)
		if !ok {
			for j := 0; j < i; j++ {
				sm.unlockExtent(streamInfo.ExtentIDs[j])
			}
			return nil, nil, errors.Errorf("%d not in stream %d no exist", extentID, srcStreamID)
		}

		newStreamInfo.ExtentIDs = append(newStreamInfo.ExtentIDs, extentID)
		if i == len(streamInfo.ExtentIDs) - 1 && exInfo.Avali == 0 { // last extent is not sealed
			exInfo.Eversion++
			exInfo.Refs++
			exInfo.SealedLength = uint64(sealedLength)
			n := len(exInfo.Replicates) + len(exInfo.Parity)
			exInfo.Avali = (1 << n) - 1
		} else {
			exInfo.Eversion++
			exInfo.Refs++
		}
		*ops = append(*ops, clientv3.OpPut(formatExtentKey(extentID), string(utils.MustMarshal(exInfo))))		
		fmt.Printf("update extent %d: %v\n", extentID, exInfo)
		updateExInfos = append(updateExInfos, exInfo)
	}
	
	*ops = append(*ops, clientv3.OpPut(formatStreamKey(destStreamID), string(utils.MustMarshal(&newStreamInfo))))


	//return failedOps, succeedOps, error
	return func(){
		unlockExtents()
	}, 
	func(){
		for _, exInfo := range updateExInfos {
			sm.extents.Set(exInfo.ExtentID, exInfo)
		}
		unlockExtents()
		sm.streams.Set(destStreamID, &newStreamInfo)
		fmt.Printf("update stream %d: %v\n", destStreamID, newStreamInfo)
	}, nil
}

func (sm *StreamManager) MultiModifySplit(ctx context.Context, req *pb.MultiModifySplitRequest) (*pb.MultiModifySplitResponse, error) {

	errDone := func(err error) (*pb.MultiModifySplitResponse, error){
		code, desCode := wire_errors.ConvertToPBCode(err)
		return &pb.MultiModifySplitResponse{
			Code: code,
			CodeDes: desCode,
		}, nil
	}

	if !sm.AmLeader() {
		return errDone(wire_errors.NotLeader)
	}

	data, _, err := etcd_utils.EtcdGetKV(sm.client, fmt.Sprintf("PART/%d", req.PartID))
	if err != nil {
		return errDone(err)
	}

	var meta pspb.PartitionMeta
	if err = meta.Unmarshal(data); err != nil {
		return errDone(err)
	}
	if bytes.Compare(req.MidKey, meta.Rg.StartKey) >= 0 && (len(meta.Rg.EndKey) == 0 || bytes.Compare(req.MidKey, meta.Rg.EndKey) < 0 ) {
	} else {
		//duplicated request ?
		return &pb.MultiModifySplitResponse{
			Code: pb.Code_OK,
		}, nil
		//return errDone(errors.Errorf("midkey is %s, not in [%s, %s)", req.MidKey, meta.Rg.StartKey, meta.Rg.EndKey))
	}

	//copy rowStream, logStream	and metaStream
	n := 1 + 3 // 1 == new partID, 2 == rowStream + logStream + metaStream
	
	var ops []clientv3.Op
	start, end, err := sm.allocUniqID(uint64(n))

	if err != nil {
		return errDone(err)
	}

	var successOps []func()
	var failedOps []func()

	//WARNING: can not change orders : log, row,meta
	f, s, err := sm.duplicateStream(&ops,  meta.LogStream, start, req.LogStreamSealedLength)
	if err != nil {
		return errDone(err)
	}
	successOps = append(successOps, s)
	failedOps = append(failedOps, f)
	

	f, s, err = sm.duplicateStream(&ops, meta.RowStream, start +1, req.RowStreamSealedLength)
	if err != nil {
		return errDone(err)
	}
	successOps = append(successOps, s)
	failedOps = append(failedOps, f)
	
	f, s, err = sm.duplicateStream(&ops, meta.MetaStream, start +2, req.MetaStreamSealedLength)
	if err != nil {
		return errDone(err)
	}
	successOps = append(successOps, s)
	failedOps = append(failedOps, f)

	
	newPartID := end - 1
	newMeta := pspb.PartitionMeta{
		LogStream: start,
		RowStream: start+1,
		MetaStream: start+2,
		Rg:&pspb.Range{StartKey: req.MidKey, EndKey: meta.Rg.EndKey},
		PartID: newPartID,
	}
	
	ops = append(ops, clientv3.OpPut(fmt.Sprintf("PART/%d", newPartID), string(utils.MustMarshal(&newMeta))))
	
	//update original meta's EndKey to midKEY
	meta.Rg.EndKey = req.MidKey
	ops = append(ops, clientv3.OpPut(fmt.Sprintf("PART/%d", req.PartID), string(utils.MustMarshal(&meta))))

	

	err = etcd_utils.EtcdSetKVS(sm.client, []clientv3.Cmp{
		clientv3.Compare(clientv3.Value(sm.leaderKey), "=", sm.memberValue),
		clientv3.Compare(clientv3.CreateRevision(req.OwnerKey), "=", req.Revision),
	}, ops)
	
	//setting ETCD failed, unlock extents
	if err != nil {
		for _, f := range failedOps {
			f()
		}
		return errDone(err)
	}
	
	//setting ETCD success, update extent/stream info in memory and unlock extents
	for _, s := range successOps {
		s()
	}

	return &pb.MultiModifySplitResponse{
		Code: pb.Code_OK,
	}, nil

}
