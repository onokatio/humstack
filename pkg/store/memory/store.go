package memory

import (
	"strings"
	"sync"
)

type MemoryStore struct {
	data      map[string]interface{}
	lockTable map[string]*sync.RWMutex
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		data:      map[string]interface{}{},
		lockTable: map[string]*sync.RWMutex{},
	}
}

func (s *MemoryStore) List(prefix string) []interface{} {
	list := []interface{}{}
	for k, obj := range s.data {
		if strings.HasPrefix(k, prefix) {
			list = append(list, obj)
		}
	}
	return list
}

func (s *MemoryStore) Get(key string) interface{} {
	if d, ok := s.data[key]; ok {
		return d
	}
	return nil
}

func (s *MemoryStore) Put(key string, data interface{}) {
	s.data[key] = data
}

func (s *MemoryStore) Delete(key string) {
	delete(s.data, key)
}

func (s *MemoryStore) Lock(key string) {
	if _, ok := s.lockTable[key]; !ok {
		s.lockTable[key] = &sync.RWMutex{}
	}

	s.lockTable[key].Lock()
}

func (s *MemoryStore) Unlock(key string) {
	if _, ok := s.lockTable[key]; ok {
		s.lockTable[key].Unlock()
	}
}
