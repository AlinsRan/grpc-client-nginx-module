// Copyright 2022 Shenzhen ZhiLiu Technology Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

/*
#cgo LDFLAGS: -shared -ldl -lpthread
#include <stdbool.h>
#include <stdint.h>
#include <stdlib.h>

typedef struct DialOpt {
    bool                     insecure;
    bool                     tls_verify;
    int                      max_recv_msg_size;
    int                      client_cert_len;
    char                    *client_cert;
    int                      client_key_len;
    char                    *client_key;
    int                      trusted_ca_len;
    char                    *trusted_ca;
} DialOpt;

typedef struct Metadata {
	int		key_len;
	int		value_len;
	char   *key;
	char   *value;
} Metadata;

typedef uintptr_t ngx_msec_t;
typedef struct CallOpt {
    ngx_msec_t timeout;

	int		   metadata_len;
	Metadata  *metadata;
} CallOpt;
*/
import "C"
import (
	"log"
	"os"
	"sync"
	"time"
	"unsafe"

	"google.golang.org/grpc"

	"github.com/api7/grpc-client-nginx-module/conn"
	"github.com/api7/grpc-client-nginx-module/task"
)

func main() {
}

func init() {
	// only keep the latest debug log
	f, err := os.OpenFile("/tmp/grpc-engine-debug.log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf(err.Error())
		return
	}
	log.Default().SetOutput(f)
	log.Default().SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
}

const (
	// buffer size allocated in the grpc-client-nginx-module
	ERR_BUF_SIZE = 512
)

type EngineCtx struct {
	c *grpc.ClientConn
}

var EngineCtxRef = sync.Map{}
var StreamRef = sync.Map{}

func reportErr(err error, errBuf unsafe.Pointer, errLen *C.size_t) {
	s := err.Error()
	if len(s) > ERR_BUF_SIZE-1 {
		s = s[:ERR_BUF_SIZE-1]
	}

	pp := (*[1 << 30]byte)(errBuf)
	copy(pp[:], s)
	*errLen = C.size_t(len(s))
}

func mustFind(m *sync.Map, ref unsafe.Pointer) interface{} {
	res, found := m.Load(ref)
	if !found {
		log.Panicf("can't find with ref %v", ref)
	}
	return res
}

//export grpc_engine_connect
func grpc_engine_connect(errBuf unsafe.Pointer, errLen *C.size_t,
	targetData unsafe.Pointer, targetLen C.int, opt *C.struct_DialOpt) unsafe.Pointer {

	target := string(C.GoBytes(targetData, targetLen))

	co := &conn.ConnectOption{
		Insecure:       bool(opt.insecure),
		TLSVerify:      bool(opt.tls_verify),
		MaxRecvMsgSize: int(opt.max_recv_msg_size),
		ClientCertFile: C.GoStringN(opt.client_cert, opt.client_cert_len),
		ClientKeyFile:  C.GoStringN(opt.client_key, opt.client_key_len),
		TrustedCA:      C.GoStringN(opt.trusted_ca, opt.trusted_ca_len),
	}
	c, err := conn.Connect(target, co)
	if err != nil {
		reportErr(err, errBuf, errLen)
		return nil
	}

	ctx := EngineCtx{}
	ctx.c = c

	// A Go function called by C code may not return a Go pointer
	var ref unsafe.Pointer = C.malloc(C.size_t(1))
	EngineCtxRef.Store(ref, &ctx)
	return ref
}

//export grpc_engine_close
func grpc_engine_close(ref unsafe.Pointer) {
	res, found := EngineCtxRef.LoadAndDelete(ref)
	if !found {
		return
	}

	ctx := res.(*EngineCtx)
	conn.Close(ctx.c)

	C.free(ref)
}

func convertMetadata(opt *C.struct_CallOpt) []string {
	if opt.metadata_len == 0 {
		return nil
	}

	md := make([]string, opt.metadata_len*2)
	pair := opt.metadata
	for i := 0; i < int(opt.metadata_len*2); i += 2 {
		md[i] = C.GoStringN(pair.key, pair.key_len)
		md[i+1] = C.GoStringN(pair.value, pair.value_len)
		pair = (*C.struct_Metadata)(unsafe.Pointer(uintptr(unsafe.Pointer(pair)) + unsafe.Sizeof(*pair)))
	}
	return md
}

//export grpc_engine_call
func grpc_engine_call(errBuf unsafe.Pointer, errLen *C.size_t,
	taskId C.long, ref unsafe.Pointer,
	methodData unsafe.Pointer, methodLen C.int,
	reqData unsafe.Pointer, reqLen C.int,
	opt *C.struct_CallOpt,
) {
	method := string(C.GoBytes(methodData, methodLen))
	req := C.GoBytes(reqData, reqLen)
	ctx := mustFind(&EngineCtxRef, ref).(*EngineCtx)
	c := ctx.c
	co := &conn.CallOption{
		Timeout:  time.Duration(opt.timeout) * time.Millisecond,
		Metadata: convertMetadata(opt),
	}

	go func() {
		out, err := conn.Call(c, method, req, co)
		task.ReportFinishedTask(uint64(taskId), out, err)
	}()
}

//export grpc_engine_new_stream
func grpc_engine_new_stream(errBuf unsafe.Pointer, errLen *C.size_t,
	sctx unsafe.Pointer, ref unsafe.Pointer,
	methodData unsafe.Pointer, methodLen C.int,
	reqData unsafe.Pointer, reqLen C.int,
	opt *C.struct_CallOpt, streamType C.int,
) {
	method := string(C.GoBytes(methodData, methodLen))
	req := C.GoBytes(reqData, reqLen)
	ctx := mustFind(&EngineCtxRef, ref).(*EngineCtx)
	c := ctx.c
	co := &conn.CallOption{
		Timeout:  time.Duration(opt.timeout) * time.Millisecond,
		Metadata: convertMetadata(opt),
	}

	go func() {
		s, err := conn.NewStream(c, method, req, co, int(streamType))
		if err != nil {
			task.ReportFinishedTask(uint64(uintptr(sctx)), nil, err)
			return
		}

		StreamRef.Store(sctx, s)
		task.ReportFinishedTask(uint64(uintptr(sctx)), nil, nil)
	}()
}

//export grpc_engine_close_stream
func grpc_engine_close_stream(sctx unsafe.Pointer) {
	res, found := StreamRef.LoadAndDelete(sctx)
	if !found {
		// stream is already closed
		return
	}

	s := res.(*conn.Stream)
	s.Close()
}

//export grpc_engine_stream_recv
func grpc_engine_stream_recv(sctx unsafe.Pointer) {
	s := mustFind(&StreamRef, sctx).(*conn.Stream)

	go func() {
		out, err := s.Recv()
		task.ReportFinishedTask(uint64(uintptr(sctx)), out, err)
	}()
}

//export grpc_engine_stream_send
func grpc_engine_stream_send(sctx unsafe.Pointer, reqData unsafe.Pointer, reqLen C.int) {
	s := mustFind(&StreamRef, sctx).(*conn.Stream)
	req := C.GoBytes(reqData, reqLen)

	go func() {
		_, err := s.Send(req)
		task.ReportFinishedTask(uint64(uintptr(sctx)), nil, err)
	}()
}

//export grpc_engine_stream_close_send
func grpc_engine_stream_close_send(sctx unsafe.Pointer) {
	s := mustFind(&StreamRef, sctx).(*conn.Stream)

	go func() {
		_, err := s.CloseSend()
		task.ReportFinishedTask(uint64(uintptr(sctx)), nil, err)
	}()
}

//export grpc_engine_free
func grpc_engine_free(ptr unsafe.Pointer) {
	C.free(ptr)
}

//export grpc_engine_wait
func grpc_engine_wait(taskNum *C.int, timeoutMSec C.int) unsafe.Pointer {
	timeout := time.Duration(int(timeoutMSec)) * time.Millisecond
	out, n := task.WaitFinishedTasks(timeout)
	*taskNum = C.int(n)
	if n == 0 {
		return nil
	}

	return C.CBytes(out)
}
