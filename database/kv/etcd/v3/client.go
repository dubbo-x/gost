/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package gxetcd

import (
	"context"
	"log"
	"sync"
	"time"
)

import (
	perrors "github.com/pkg/errors"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/clientv3/concurrency"
	"google.golang.org/grpc"
)

var (
	// ErrNilETCDV3Client raw client nil
	ErrNilETCDV3Client = perrors.New("etcd raw client is nil") // full describe the ERR
	// ErrKVPairNotFound not found key
	ErrKVPairNotFound = perrors.New("k/v pair not found")
)

// NewConfigClient create new Client
func NewConfigClient(opts ...Option) *Client {
	options := &Options{
		Heartbeat: 1, // default Heartbeat
	}
	for _, opt := range opts {
		opt(options)
	}

	newClient, err := NewClient(options.Name, options.Endpoints, options.Timeout, options.Heartbeat)
	if err != nil {
		log.Printf("new etcd client (Name{%s}, etcd addresses{%v}, Timeout{%d}) = error{%v}",
			options.Name, options.Endpoints, options.Timeout, err)
	}
	return newClient
}

// Client represents etcd client Configuration
type Client struct {
	lock     sync.RWMutex
	quitOnce sync.Once

	// these properties are only set once when they are started.
	name      string
	endpoints []string
	timeout   time.Duration
	heartbeat int

	ctx       context.Context    // if etcd server connection lose, the ctx.Done will be sent msg
	cancel    context.CancelFunc // cancel the ctx, all watcher will stopped
	rawClient *clientv3.Client

	exit chan struct{}
	Wait sync.WaitGroup
}

// NewClient create a client instance with name, endpoints etc.
func NewClient(name string, endpoints []string, timeout time.Duration, heartbeat int) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())

	rawClient, err := clientv3.New(clientv3.Config{
		Context:     ctx,
		Endpoints:   endpoints,
		DialTimeout: timeout,
		DialOptions: []grpc.DialOption{grpc.WithBlock()},
	})
	if err != nil {
		cancel()
		return nil, perrors.WithMessage(err, "new raw client block connect to server")
	}

	c := &Client{
		name:      name,
		timeout:   timeout,
		endpoints: endpoints,
		heartbeat: heartbeat,

		ctx:       ctx,
		cancel:    cancel,
		rawClient: rawClient,

		exit: make(chan struct{}),
	}

	if err := c.keepSession(); err != nil {
		cancel()
		return nil, perrors.WithMessage(err, "client keep session")
	}
	return c, nil
}

// NOTICE: need to get the lock before calling this method
func (c *Client) clean() {
	// close raw client
	c.rawClient.Close()

	// cancel ctx for raw client
	c.cancel()

	// clean raw client
	c.rawClient = nil
}

func (c *Client) stop() bool {
	select {
	case <-c.exit:
		return false
	default:
		ret := false
		c.quitOnce.Do(func() {
			ret = true
			close(c.exit)
		})
		return ret
	}
}

// GetCtx return client context
func (c *Client) GetCtx() context.Context {
	return c.ctx
}

// Close close client
func (c *Client) Close() {
	if c == nil {
		return
	}

	// stop the client
	if ret := c.stop(); !ret {
		return
	}

	// wait client keep session stop
	c.Wait.Wait()

	c.lock.Lock()
	defer c.lock.Unlock()
	if c.rawClient != nil {
		c.clean()
	}
	log.Printf("etcd client{Name:%s, Endpoints:%s} exit now.", c.name, c.endpoints)
}

func (c *Client) keepSession() error {
	s, err := concurrency.NewSession(c.rawClient, concurrency.WithTTL(c.heartbeat))
	if err != nil {
		return perrors.WithMessage(err, "new session with server")
	}

	// must add wg before go keep session goroutine
	c.Wait.Add(1)
	go c.keepSessionLoop(s)
	return nil
}

func (c *Client) keepSessionLoop(s *concurrency.Session) {
	defer func() {
		c.Wait.Done()
		log.Printf("etcd client {Endpoints:%v, Name:%s} keep goroutine game over.", c.endpoints, c.name)
	}()

	for {
		select {
		case <-c.Done():
			// Client be stopped, will clean the client hold resources
			return
		case <-s.Done():
			log.Print("etcd server stopped")
			c.lock.Lock()
			// when etcd server stopped, cancel ctx, stop all watchers
			c.clean()
			// when connection lose, stop client, trigger reconnect to etcd
			c.stop()
			c.lock.Unlock()
			return
		}
	}
}

// GetRawClient return etcd raw client
func (c *Client) GetRawClient() *clientv3.Client {
	c.lock.RLock()
	defer c.lock.RUnlock()

	return c.rawClient
}

// GetEndPoints return etcd endpoints
func (c *Client) GetEndPoints() []string {
	return c.endpoints
}

// if k not exist will put k/v in etcd, otherwise return nil
func (c *Client) put(k string, v string, opts ...clientv3.OpOption) error {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return ErrNilETCDV3Client
	}

	_, err := rawClient.Txn(c.ctx).
		If(clientv3.Compare(clientv3.Version(k), "<", 1)).
		Then(clientv3.OpPut(k, v, opts...)).
		Commit()
	return err
}

// if k not exist will put k/v in etcd
// if k is already exist in etcd, replace it
func (c *Client) update(k string, v string, opts ...clientv3.OpOption) error {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return ErrNilETCDV3Client
	}

	_, err := rawClient.Txn(c.ctx).
		If(clientv3.Compare(clientv3.Version(k), "!=", -1)).
		Then(clientv3.OpPut(k, v, opts...)).
		Commit()
	return err
}

func (c *Client) delete(k string) error {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return ErrNilETCDV3Client
	}

	_, err := rawClient.Delete(c.ctx, k)
	return err
}

func (c *Client) get(k string) (string, error) {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return "", ErrNilETCDV3Client
	}

	resp, err := rawClient.Get(c.ctx, k)
	if err != nil {
		return "", err
	}

	if len(resp.Kvs) == 0 {
		return "", ErrKVPairNotFound
	}

	return string(resp.Kvs[0].Value), nil
}

// CleanKV delete all key and value
func (c *Client) CleanKV() error {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return ErrNilETCDV3Client
	}

	_, err := rawClient.Delete(c.ctx, "", clientv3.WithPrefix())
	return err
}

// GetChildren return node children
func (c *Client) GetChildren(k string) ([]string, []string, error) {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return nil, nil, ErrNilETCDV3Client
	}

	resp, err := rawClient.Get(c.ctx, k, clientv3.WithPrefix())
	if err != nil {
		return nil, nil, err
	}

	if len(resp.Kvs) == 0 {
		return nil, nil, ErrKVPairNotFound
	}

	kList := make([]string, 0, len(resp.Kvs))
	vList := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		kList = append(kList, string(kv.Key))
		vList = append(vList, string(kv.Value))
	}
	return kList, vList, nil
}

func (c *Client) watchWithPrefix(prefix string) (clientv3.WatchChan, error) {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return nil, ErrNilETCDV3Client
	}

	return rawClient.Watch(c.ctx, prefix, clientv3.WithPrefix()), nil
}

func (c *Client) watch(k string) (clientv3.WatchChan, error) {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return nil, ErrNilETCDV3Client
	}

	return rawClient.Watch(c.ctx, k), nil
}

func (c *Client) keepAliveKV(k string, v string) error {
	rawClient := c.GetRawClient()

	if rawClient == nil {
		return ErrNilETCDV3Client
	}

	// make lease time longer, since 1 second is too short
	lease, err := rawClient.Grant(c.ctx, int64(30*time.Second.Seconds()))
	if err != nil {
		return perrors.WithMessage(err, "grant lease")
	}

	keepAlive, err := rawClient.KeepAlive(c.ctx, lease.ID)
	if err != nil || keepAlive == nil {
		rawClient.Revoke(c.ctx, lease.ID)
		if err != nil {
			return perrors.WithMessage(err, "keep alive lease")
		}
		return perrors.New("keep alive lease")
	}

	_, err = rawClient.Put(c.ctx, k, v, clientv3.WithLease(lease.ID))
	return perrors.WithMessage(err, "put k/v with lease")
}

// Done return exit chan
func (c *Client) Done() <-chan struct{} {
	return c.exit
}

// Valid check client
func (c *Client) Valid() bool {
	select {
	case <-c.exit:
		return false
	default:
	}

	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.rawClient != nil
}

// Create key value ...
func (c *Client) Create(k string, v string) error {
	err := c.put(k, v)
	return perrors.WithMessagef(err, "put k/v (key: %s value %s)", k, v)
}

// Update key value ...
func (c *Client) Update(k, v string) error {
	err := c.update(k, v)
	return perrors.WithMessagef(err, "Update k/v (key: %s value %s)", k, v)
}

// Delete key
func (c *Client) Delete(k string) error {
	err := c.delete(k)
	return perrors.WithMessagef(err, "delete k/v (key %s)", k)
}

// RegisterTemp registers a temporary node
func (c *Client) RegisterTemp(k, v string) error {
	err := c.keepAliveKV(k, v)
	return perrors.WithMessagef(err, "keepalive kv (key %s)", k)
}

// GetChildrenKVList gets children kv list by @k
func (c *Client) GetChildrenKVList(k string) ([]string, []string, error) {
	kList, vList, err := c.GetChildren(k)
	return kList, vList, perrors.WithMessagef(err, "get key children (key %s)", k)
}

// Get gets value by @k
func (c *Client) Get(k string) (string, error) {
	v, err := c.get(k)
	return v, perrors.WithMessagef(err, "get key value (key %s)", k)
}

// Watch watches on spec key
func (c *Client) Watch(k string) (clientv3.WatchChan, error) {
	wc, err := c.watch(k)
	return wc, perrors.WithMessagef(err, "watch prefix (key %s)", k)
}

// WatchWithPrefix watches on spec prefix
func (c *Client) WatchWithPrefix(prefix string) (clientv3.WatchChan, error) {
	wc, err := c.watchWithPrefix(prefix)
	return wc, perrors.WithMessagef(err, "watch prefix (key %s)", prefix)
}
