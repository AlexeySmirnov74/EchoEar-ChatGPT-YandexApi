package ai

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

var errBackgroundStaleSession = errors.New("superseded device session")
var backgroundMagic = []byte("ECHOIMG1")

// A session lease prevents an old, half-open websocket from replacing a new
// connection's state. The lock also serializes writes for the same device.
type backgroundStore struct {
	mu sync.Mutex
	ioMu sync.Mutex
	dir string
	next uint64
	sessions map[string]*atomic.Uint64
	beforeWrite func(bool) // test hook; nil in production
}
var speakerBackgrounds = &backgroundStore{sessions:map[string]*atomic.Uint64{}}

func (s *backgroundStore) path(device string) (string,error) {
	if strings.TrimSpace(device)=="" || len(device)>256 { return "",errors.New("invalid device id") }
	sum:=sha256.Sum256([]byte(device)); dir:=s.dir
	if dir=="" { dir=filepath.Join(stackChanDataDir(),"image-backgrounds") }
	return filepath.Join(dir,hex.EncodeToString(sum[:])+".bin"),nil
}
func (s *backgroundStore) acquire(device string)(uint64,error) {
	if _,err:=s.path(device);err!=nil{return 0,err}
	s.mu.Lock(); defer s.mu.Unlock()
	if s.sessions==nil {s.sessions=map[string]*atomic.Uint64{}}
	if s.sessions[device]==nil {s.sessions[device]=&atomic.Uint64{}}
	s.next++; if s.next==0 {s.next++}; s.sessions[device].Store(s.next)
	return s.next,nil
}
func (s *backgroundStore) release(device string, lease uint64) {
	s.mu.Lock();p:=s.sessions[device];s.mu.Unlock();if p!=nil {p.CompareAndSwap(lease,0)}
}
func (s *backgroundStore) current(device string,lease uint64) bool {
	s.mu.Lock();p:=s.sessions[device];s.mu.Unlock();return lease!=0 && p!=nil && p.Load()==lease
}
func (s *backgroundStore) leaseRef(device string)*atomic.Uint64 {s.mu.Lock();defer s.mu.Unlock();return s.sessions[device]}
func (s *backgroundStore) read(device string,lease uint64)([]byte,error) {
	s.ioMu.Lock(); defer s.ioMu.Unlock()
	if !s.current(device,lease) {return nil,errBackgroundStaleSession}
	path,err:=s.path(device); if err!=nil{return nil,err}
	f,err:=os.Open(path); if os.IsNotExist(err){return nil,nil}; if err!=nil{return nil,err}; defer f.Close()
	data,err:=io.ReadAll(io.LimitReader(f,int64(len(backgroundMagic)+sha256.Size+imageFrameBytes+1))); if err!=nil{return nil,err}
	// An empty payload is an atomically written tombstone after clear.
	if bytes.Equal(data,backgroundMagic) { return nil,nil }
	// A previous operation was interrupted before durable confirmation. Never
	// resurrect the old background after an uncertain clear.
	if bytes.Equal(data,append(append([]byte(nil),backgroundMagic...), 'P')) {return nil,nil}
	if len(data)!=len(backgroundMagic)+sha256.Size+imageFrameBytes || !bytes.Equal(data[:len(backgroundMagic)],backgroundMagic) {return nil,errors.New("invalid background file")}
	pixels:=data[len(backgroundMagic)+sha256.Size:]; sum:=sha256.Sum256(pixels)
	if !bytes.Equal(data[len(backgroundMagic):len(backgroundMagic)+sha256.Size],sum[:]) {return nil,errors.New("invalid background checksum")}
	return pixels,nil
}
func (s *backgroundStore) write(device string,lease uint64,pixels []byte) error {
	if pixels!=nil && len(pixels)!=imageFrameBytes {return errors.New("invalid background frame")}
	return s.writeRecord(device,lease,pixels,false)
}
func (s *backgroundStore) writePending(device string,lease uint64) error {return s.writeRecord(device,lease,nil,true)}
func (s *backgroundStore) writeRecord(device string,lease uint64,pixels []byte,pending bool) error {
	if s.beforeWrite!=nil {s.beforeWrite(pending)}
	s.ioMu.Lock(); defer s.ioMu.Unlock()
	if !s.current(device,lease){return errBackgroundStaleSession}
	path,err:=s.path(device);if err!=nil{return err}
	if err=os.MkdirAll(filepath.Dir(path),0700);err!=nil{return err}
	f,err:=os.CreateTemp(filepath.Dir(path),".background-*");if err!=nil{return err}
	name:=f.Name(); defer os.Remove(name)
	data:=append([]byte(nil),backgroundMagic...)
	if pending {data=append(data,'P')} else if pixels!=nil {sum:=sha256.Sum256(pixels); data=append(data,sum[:]...);data=append(data,pixels...)}
	if err=f.Chmod(0600);err==nil {_,err=f.Write(data)};if err==nil {err=f.Sync()};closeErr:=f.Close();if err==nil {err=closeErr};if err!=nil{return err}
	// Hold the short-lived lease mutex only across rename so superseded
	// sessions cannot commit. Actor code never waits on this disk mutex.
	s.mu.Lock();p:=s.sessions[device];if lease==0||p==nil||p.Load()!=lease{s.mu.Unlock();return errBackgroundStaleSession};err=os.Rename(name,path);s.mu.Unlock();if err!=nil{return err}
	d,err:=os.Open(filepath.Dir(path));if err!=nil{return err};defer d.Close();if err=d.Sync();err!=nil{return fmt.Errorf("sync background directory: %w",err)}
	return nil
}
