package ai

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"time"
)

var errBackgroundDiskTimeout=errors.New("background storage unavailable or timed out")

type backgroundDiskResult struct { operation string; token uint64; pixels []byte; err error; lease uint64; leaseRef *atomic.Uint64 }
type backgroundDiskJob struct {
	operation string
	token uint64
	device string
	lease uint64
	store *backgroundStore
	pixels []byte
	deadline time.Time
	reply chan<- backgroundDiskResult
	ctx context.Context
}
var backgroundDiskJobs = make(chan backgroundDiskJob, 8)
var backgroundID atomic.Uint32

func nextBackgroundID() int { return int(backgroundID.Add(1)%2147483646)+1 }

func init() { var seed [4]byte; if _,err:=rand.Read(seed[:]);err==nil {backgroundID.Store(binary.LittleEndian.Uint32(seed[:])%2147483646)}; go func() { for job:=range backgroundDiskJobs {
	result:=backgroundDiskResult{operation:job.operation,token:job.token}
	if time.Now().After(job.deadline) {result.err=errBackgroundDiskTimeout} else {switch job.operation {
	case "open":
		result.lease,result.err=job.store.acquire(job.device)
		if result.err==nil {result.leaseRef=job.store.leaseRef(job.device);result.pixels,result.err=job.store.read(job.device,result.lease)}
	case "read":result.pixels,result.err=job.store.read(job.device,job.lease)
	case "prepare":result.err=job.store.writePending(job.device,job.lease)
	case "commit":result.err=job.store.write(job.device,job.lease,job.pixels)
	default:result.err=errBackgroundDiskTimeout
	}}
	if job.operation=="open" {
		select {case job.reply<-result:case <-job.ctx.Done():if result.lease!=0 {job.store.release(job.device,result.lease)}}
	} else {select {case job.reply<-result:default:}}
} }() }

func submitBackgroundDisk(job backgroundDiskJob) bool {select {case backgroundDiskJobs<-job:return true;default:return false}}
