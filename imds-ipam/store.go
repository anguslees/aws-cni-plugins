// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

const storefile = "data.json"
const lockfile = "lock"

var ErrAlreadyReserved = errors.New("IP is already allocated")

// Super-inefficient datastructure. I look forward to the day when
// this becomes an issue.
type StoreRow struct {
	ID     string `json:"id"`
	IfName string `json:"ifname"`
	IP     string `json:"ip"` // net.IP doesn't serialize. boo.
}

type Store struct {
	dir          string
	data         []StoreRow
	checkpointer Checkpointer
	lockFile     *os.File
}

func NewStore(dir string) Store {
	return Store{
		dir:          dir,
		checkpointer: NewJSONFile(filepath.Join(dir, storefile)),
	}
}

func (s *Store) ReserveIP(id, ifname string, ip net.IP) error {
	ipstr := ip.String()
	for _, row := range s.data {
		if row.IP == ipstr {
			return ErrAlreadyReserved
		}
	}

	s.data = append(s.data, StoreRow{ID: id, IfName: ifname, IP: ipstr})
	return nil
}

func (s *Store) FindByID(id, ifname string) *net.IP {
	for _, row := range s.data {
		if row.ID == id && row.IfName == ifname {
			ip := net.ParseIP(row.IP)
			return &ip
		}
	}

	return nil
}

func (s *Store) ReleaseID(id, ifname string) {
	for i, row := range s.data {
		if row.ID == id && row.IfName == ifname {
			// Found! Remove element (does not preserve order)
			s.data[i] = s.data[len(s.data)-1]
			s.data = s.data[:len(s.data)-1]
			break
		}
	}
}

func (s *Store) lock() error {
	if s.lockFile != nil {
		panic("lock() called when already locked")
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", s.dir, err)
	}

	f, err := os.OpenFile(filepath.Join(s.dir, lockfile), os.O_RDONLY|os.O_CREATE, 0755)
	if err != nil {
		return err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return err
	}

	s.lockFile = f

	return nil
}

func (s *Store) unlock() error {
	if err := s.lockFile.Close(); err != nil {
		return err
	}
	s.lockFile = nil
	return nil
}

func (s *Store) Open() error {
	if err := s.lock(); err != nil {
		return err
	}

	if err := s.checkpointer.Restore(&s.data); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}

	return nil
}

func (s *Store) Close() error {
	if err := s.checkpointer.Checkpoint(s.data); err != nil {
		return err
	}

	return s.unlock()
}
