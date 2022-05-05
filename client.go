package gorpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"gorpc/codec"
	"log"
	"net"
	"sync"
)

type Call struct {
	Seq           uint64
	ServiceMethod string
	Args          interface{}
	Reply         interface{}
	Error         error
	Done          chan *Call
}

//
// done
// @Description: 支持异步调用，当调用结束的时候通知调用方
// @receiver call
//
func (call *Call) done() {
	call.Done <- call
}

type Client struct {
	cc      codec.Codec
	opt     *Option
	sending sync.Mutex
	header  codec.Header
	mu      sync.Mutex
	seq     uint64
	pending map[uint64]*Call
	//表示用户主动关闭Client
	closing bool
	//表示有错误发生关闭Client
	shutdown bool
}

var ErrShutdown = errors.New("connection is shutdown")
var ErrClosing = errors.New("connection is closing")

//
// Close
// @Description: 关闭连接
// @receiver client
// @return error
//
func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing {
		return ErrClosing
	}
	client.closing = true
	return client.cc.Close()
}

//
// IsAvailable
// @Description: 查看Client是否可用
// @receiver client
// @return bool
//
func (client *Client) IsAvailable() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return !client.shutdown && !client.closing
}

//
// registerCall
// @Description: 在Client中注册Call
// @receiver client
// @param call
// @return uint64
// @return error
//
func (client *Client) registerCall(call *Call) (uint64, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing {
		return 0, ErrClosing
	} else if client.shutdown {
		log.Println("rpc client:client shutdown")
		return 0, ErrShutdown
	}
	call.Seq = client.seq
	client.pending[call.Seq] = call
	client.seq++
	return call.Seq, nil
}

//
// removeCall
// @Description: 在Client删除并返回指定Call
// @receiver client
// @param seq
// @return *Call
//
func (client *Client) removeCall(seq uint64) *Call {
	client.mu.Lock()
	defer client.mu.Unlock()
	call := client.pending[seq]
	delete(client.pending, seq)
	return call
}

//
// terminateCalls
// @Description: 通知所有call队列有客户端或者服务端调用错误发生
// @receiver client
// @param err
//
func (client *Client) terminateCalls(err error) {
	client.sending.Lock()
	defer client.sending.Unlock()
	client.mu.Lock()
	defer client.mu.Unlock()
	client.shutdown = true
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
}

//
// receive
// @Description:
// @receiver client
//
//func (client *Client) receive() {
//	var err error
//	for err == nil {
//		var h codec.Header
//		if err = client.cc.ReadHeader(&h); err != nil {
//			break
//		}
//		call := client.removeCall(h.Seq)
//		switch {
//		case call == nil:
//			err = client.cc.ReadBody(nil)
//		case h.Error != "":
//			call.Error = fmt.Errorf(h.Error)
//			err = client.cc.ReadBody(nil)
//			call.done()
//		default:
//			err = client.cc.ReadBody(call.Reply)
//			if err != nil {
//				call.Error = errors.New("reading body " + err.Error())
//			}
//			call.done()
//		}
//	}
//	client.terminateCalls(err)
//}

func (client *Client) receive() {
	var err error
	for err == nil {
		var h codec.Header
		if err = client.cc.ReadHeader(&h); err != nil {
			fmt.Println(err)
			log.Println("rpc client:header decode errors happen")
			break
		}
		call := client.removeCall(h.Seq)
		switch {
		case call == nil:
			// it usually means that Write partially failed
			// and call was already removed.
			err = client.cc.ReadBody(nil)
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			err = client.cc.ReadBody(nil)
			call.done()
		default:
			err = client.cc.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}

	// error occurs, so terminateCalls pending calls
	client.terminateCalls(err)
}

//
// NewClient
// @Description: 创建Client实例，首先协商编码协议，用receive函数接受回应
// @param conn
// @param opt
// @return *Client
// @return error
//
func NewClient(conn net.Conn, opt *Option) (*Client, error) {
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid codec type %s", opt.CodecType)
		log.Println("rpc client:codec error:", err)
		return nil, err
	}
	//给server发送option信息
	if err := json.NewEncoder(conn).Encode(opt); err != nil {
		log.Println("rpc client:options error:", err)
		_ = conn.Close()
		return nil, err
	}
	return newClientCodec(f(conn), opt), nil

}

//
// newClientCodec
// @Description: 初始化Client编码器
// @param cc
// @param opt
// @return *Client
//
func newClientCodec(cc codec.Codec, opt *Option) *Client {
	client := &Client{
		seq:     1,
		cc:      cc,
		opt:     opt,
		pending: make(map[uint64]*Call),
	}
	go client.receive()
	log.Println("rpc client:shutdown status is ", client.shutdown)
	log.Println("rpc client:init client success")
	return client
}

//
// parseOptions
// @Description: 封装Option
// @param opts
// @return *Option
// @return error
//
func parseOptions(opts ...*Option) (*Option, error) {
	if len(opts) == 0 || opts[0] == nil {
		return DefaultOption, nil
	}
	if len(opts) != 1 {
		return nil, errors.New("number of options is more than 1")
	}
	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}
	return opt, nil
}

//
// Dial
// @Description: 用户传入服务端地址，创建Client实例
// @param network
// @param address
// @param opts
// @return client
// @return err
//
func Dial(network, address string, opts ...*Option) (client *Client, err error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close()
	}()
	return NewClient(conn, opt)
}

//
// send
// @Description: Client发送请求
// @receiver client
// @param call
//
func (client *Client) send(call *Call) {
	//加锁保证发送完整请求
	client.sending.Lock()
	defer client.sending.Unlock()
	//log.Println("rpc server:shutdown status is ", client.shutdown)
	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}
	//预备request
	client.header.ServiceMethod = call.ServiceMethod
	client.header.Seq = seq
	client.header.Error = ""

	//发送header
	if err := client.cc.Write(&client.header, call.Args); err != nil {
		call := client.removeCall(seq)
		if call != nil {
			call.Error = err
			call.done()
		}
	}

}

func (client *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("rpc client:done chanel is unbuffered")
	}
	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

func (client *Client) Call(serviceMethod string, args, reply interface{}) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	return call.Error
}
