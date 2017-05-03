// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"sync"

	"github.com/zengnotes/redis-proxy/codis/models"
	"github.com/zengnotes/redis-proxy/codis/utils/errors"
	"github.com/zengnotes/redis-proxy/codis/utils/logger"
	"github.com/zengnotes/redis-proxy/config"
)

const MaxSlotNum = models.MaxSlotNum

type Router struct {
	mu     sync.RWMutex

	pool   struct {
			   primary *sharedBackendConnPool
			   replica *sharedBackendConnPool
		   }
	slots  [MaxSlotNum]Slot

	config *config.Config
	online bool
	closed bool
}

//Set backend parallel connections per server

func NewRouter(config *config.Config) *Router {
	s := &Router{config: config}
	//设置每个服务器的后端并行连接
	s.pool.primary = newSharedBackendConnPool(config, config.BackendPrimaryParallel)
	//复制后端
	s.pool.replica = newSharedBackendConnPool(config, config.BackendReplicaParallel)
	//MaxSlotNum 创建
	for i := range s.slots {
		s.slots[i].id = i
		s.slots[i].method = &forwardSync{}
	}
	return s
}

func (s *Router) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.online = true
}

func (s *Router) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	for i := range s.slots {
		s.fillSlot(&models.Slot{Id: i}, false, nil)
	}
}

func (s *Router) GetSlots() []*models.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slots := make([]*models.Slot, MaxSlotNum)
	for i := range s.slots {
		slots[i] = s.slots[i].snapshot()
	}
	return slots
}

func (s *Router) GetSlot(id int) *models.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id < 0 || id >= MaxSlotNum {
		return nil
	}
	slot := &s.slots[id]
	return slot.snapshot()
}

func (s *Router) HasSwitched() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.slots {
		if s.slots[i].switched {
			return true
		}
	}
	return false
}

var (
	ErrClosedRouter = errors.New("use of closed router")
	ErrInvalidSlotId = errors.New("use of invalid slot id")
	ErrInvalidMethod = errors.New("use of invalid forwarder method")
)

func (s *Router) FillSlot(m *models.Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedRouter
	}
	if m.Id < 0 || m.Id >= MaxSlotNum {
		return ErrInvalidSlotId
	}
	var method forwardMethod
	switch m.ForwardMethod {
	default:
		return ErrInvalidMethod
	case models.ForwardSync:
		method = &forwardSync{}
	case models.ForwardSemiAsync:
		method = &forwardSemiAsync{}
	}
	s.fillSlot(m, false, method)
	return nil
}

func (s *Router) KeepAlive() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrClosedRouter
	}
	s.pool.primary.KeepAlive()
	s.pool.replica.KeepAlive()
	return nil
}

func (s *Router) isOnline() bool {
	return s.online && !s.closed
}
//Hash 转发
func (s *Router) dispatch(r *Request) error {
	hkey := getHashKey(r.Multi, r.OpStr)
	var id = Hash(hkey) % MaxSlotNum
	slot := &s.slots[id]
	config.Logger.Logf(logger.DEBUG, "dispatch to slot slot.backend.bc=%v", slot.backend.bc)
	return slot.forward(r, hkey)
}

func (s *Router) dispatchSlot(r *Request, id int) error {
	if id < 0 || id >= MaxSlotNum {
		return ErrInvalidSlotId
	}
	slot := &s.slots[id]
	return slot.forward(r, nil)
}

func (s *Router) dispatchAddr(r *Request, addr string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if bc := s.pool.primary.Get(addr).BackendConn(r.Database, r.Seed16(), false); bc != nil {
		bc.PushBack(r)
		return true
	}
	if bc := s.pool.replica.Get(addr).BackendConn(r.Database, r.Seed16(), false); bc != nil {
		bc.PushBack(r)
		return true
	}
	return false
}

//添加节点
func (s *Router) fillSlot(m *models.Slot, switched bool, method forwardMethod) {
	slot := &s.slots[m.Id]
	slot.blockAndWait()

	slot.backend.bc.Release()
	slot.backend.bc = nil
	slot.backend.id = 0
	slot.migrate.bc.Release()
	slot.migrate.bc = nil
	slot.migrate.id = 0
	for i := range slot.replicaGroups {
		for _, bc := range slot.replicaGroups[i] {
			bc.Release()
		}
	}
	slot.replicaGroups = nil

	slot.switched = switched

	if addr := m.BackendAddr; len(addr) != 0 {
		slot.backend.bc = s.pool.primary.Retain(addr)
		slot.backend.id = m.BackendAddrGroupId
	}
	if from := m.MigrateFrom; len(from) != 0 {
		slot.migrate.bc = s.pool.primary.Retain(from)
		slot.migrate.id = m.MigrateFromGroupId
	}
	if !s.config.BackendPrimaryOnly {
		for i := range m.ReplicaGroups {
			var group []*sharedBackendConn
			for _, addr := range m.ReplicaGroups[i] {
				group = append(group, s.pool.replica.Retain(addr))
			}
			if len(group) == 0 {
				continue
			}
			slot.replicaGroups = append(slot.replicaGroups, group)
		}
	}
	if method != nil {
		slot.method = method
	}

	if !m.Locked {
		slot.unblock()
	}
	if !s.closed {
		if slot.migrate.bc != nil {
			if switched {
				config.Logger.Logf(logger.WARNING, "fill slot %04d, backend.addr = %s, migrate.from = %s, locked = %t, +switched",
					slot.id, slot.backend.bc.Addr(), slot.migrate.bc.Addr(), slot.lock.hold)
			} else {
				config.Logger.Logf(logger.WARNING, "fill slot %04d, backend.addr = %s, migrate.from = %s, locked = %t",
					slot.id, slot.backend.bc.Addr(), slot.migrate.bc.Addr(), slot.lock.hold)
			}
		} else {
			if switched {
				config.Logger.Logf(logger.WARNING, "fill slot %04d, backend.addr = %s, locked = %t, +switched",
					slot.id, slot.backend.bc.Addr(), slot.lock.hold)
			} else {
				config.Logger.Logf(logger.WARNING, "fill slot %04d, backend.addr = %s, locked = %t",
					slot.id, slot.backend.bc.Addr(), slot.lock.hold)
			}
		}
	}
}
/*
func (s *Router) SwitchMasters(masters map[int]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosedRouter
	}
	cache := &redis.InfoCache{
		Auth: s.config.ProductAuth, Timeout: time.Millisecond * 100,
	}
	for i := range s.slots {
		s.trySwitchMaster(i, masters, cache)
	}
	return nil
}

func (s *Router) trySwitchMaster(id int, masters map[int]string, cache *redis.InfoCache) {
	var switched bool
	var m = s.slots[id].snapshot()

	hasSameRunId := func(addr1, addr2 string) bool {
		if addr1 != addr2 {
			rid1 := cache.GetRunId(addr1)
			rid2 := cache.GetRunId(addr2)
			return rid1 != "" && rid1 == rid2
		}
		return true
	}

	if addr := masters[m.BackendAddrGroupId]; addr != "" {
		if !hasSameRunId(addr, m.BackendAddr) {
			m.BackendAddr, switched = addr, true
		}
	}
	if addr := masters[m.MigrateFromGroupId]; addr != "" {
		if !hasSameRunId(addr, m.MigrateFrom) {
			m.MigrateFrom, switched = addr, true
		}
	}
	if switched {
		s.fillSlot(m, true, nil)
	}
}
*/
