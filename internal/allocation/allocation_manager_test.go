package allocation

import (
	"io"
	"math/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gortc/turn"
	"github.com/pion/logging"
	"github.com/pion/turn/internal/ipnet"
)

func TestManager(t *testing.T) {
	tt := []struct {
		name string
		f    func(*testing.T, ipnet.PacketConn)
	}{
		{"CreateInvalidAllocation", subTestCreateInvalidAllocation},
		{"CreateAllocation", subTestCreateAllocation},
		{"CreateAllocationDuplicateFiveTuple", subTestCreateAllocationDuplicateFiveTuple},
		{"DeleteAllocation", subTestDeleteAllocation},
		{"AllocationTimeout", subTestAllocationTimeout},
		{"Close", subTestManagerClose},
	}

	network := "udp4"
	c, err := net.ListenPacket(network, "0.0.0.0:0")
	if err != nil {
		panic(err)
	}

	turnSocket, err := ipnet.NewPacketConn(network, c)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range tt {
		f := tc.f
		t.Run(tc.name, func(t *testing.T) {
			f(t, turnSocket)
		})
	}
}

// test invalid Allocation creations
func subTestCreateInvalidAllocation(t *testing.T, turnSocket ipnet.PacketConn) {
	m := newTestManager()
	if a, err := m.CreateAllocation(nil, turnSocket, net.IPv4zero, 0, turn.DefaultLifetime); a != nil || err == nil {
		t.Errorf("Illegally created allocation with nil FiveTuple")
	}
	if a, err := m.CreateAllocation(randomFiveTuple(), nil, net.IPv4zero, 0, turn.DefaultLifetime); a != nil || err == nil {
		t.Errorf("Illegally created allocation with nil turnSocket")
	}
	if a, err := m.CreateAllocation(randomFiveTuple(), turnSocket, net.IPv4zero, 0, 0); a != nil || err == nil {
		t.Errorf("Illegally created allocation with 0 lifetime")
	}
}

// test valid Allocation creations
func subTestCreateAllocation(t *testing.T, turnSocket ipnet.PacketConn) {
	m := newTestManager()
	fiveTuple := randomFiveTuple()
	if a, err := m.CreateAllocation(fiveTuple, turnSocket, net.IPv4zero, 0, turn.DefaultLifetime); a == nil || err != nil {
		t.Errorf("Failed to create allocation %v %v", a, err)
	}

	if a := m.GetAllocation(fiveTuple); a == nil {
		t.Errorf("Failed to get allocation right after creation")
	}
}

// test that two allocations can't be created with the same FiveTuple
func subTestCreateAllocationDuplicateFiveTuple(t *testing.T, turnSocket ipnet.PacketConn) {
	m := newTestManager()
	fiveTuple := randomFiveTuple()
	if a, err := m.CreateAllocation(fiveTuple, turnSocket, net.IPv4zero, 0, turn.DefaultLifetime); a == nil || err != nil {
		t.Errorf("Failed to create allocation %v %v", a, err)
	}

	if a, err := m.CreateAllocation(fiveTuple, turnSocket, net.IPv4zero, 0, turn.DefaultLifetime); a != nil || err == nil {
		t.Errorf("Was able to create allocation with same FiveTuple twice")
	}
}

func subTestDeleteAllocation(t *testing.T, turnSocket ipnet.PacketConn) {
	m := newTestManager()
	fiveTuple := randomFiveTuple()
	if a, err := m.CreateAllocation(fiveTuple, turnSocket, net.IPv4zero, 0, turn.DefaultLifetime); a == nil || err != nil {
		t.Errorf("Failed to create allocation %v %v", a, err)
	}

	if a := m.GetAllocation(fiveTuple); a == nil {
		t.Errorf("Failed to get allocation right after creation")
	}

	m.DeleteAllocation(fiveTuple)
	if a := m.GetAllocation(fiveTuple); a != nil {
		t.Errorf("Get allocation with %v should be nil after delete", fiveTuple)
	}
}

// test that allocation should be closed if timeout
func subTestAllocationTimeout(t *testing.T, turnSocket ipnet.PacketConn) {
	m := newTestManager()
	allocations := make([]*Allocation, 5)
	lifetime := time.Second

	for index := range allocations {
		fiveTuple := randomFiveTuple()

		a, err := m.CreateAllocation(fiveTuple, turnSocket, net.IPv4zero, 0, lifetime)
		if err != nil {
			t.Errorf("Failed to create allocation with %v", fiveTuple)
		}

		allocations[index] = a
	}

	// make sure all allocations timeout
	time.Sleep(lifetime + time.Second)
	for _, alloc := range allocations {
		if !isClose(alloc.RelaySocket) {
			t.Error("Allocation relay socket should be closed if lifetime timeout")
		}
	}
}

// test for manager close
func subTestManagerClose(t *testing.T, turnSocket ipnet.PacketConn) {
	m := newTestManager()
	allocations := make([]*Allocation, 2)

	a1, _ := m.CreateAllocation(randomFiveTuple(), turnSocket, net.IPv4zero, 0, time.Second)
	allocations[0] = a1
	a2, _ := m.CreateAllocation(randomFiveTuple(), turnSocket, net.IPv4zero, 0, time.Minute)
	allocations[1] = a2

	// make a1 timeout
	time.Sleep(2 * time.Second)

	if err := m.Close(); err != nil {
		t.Errorf("Manager close with error: %v", err)
	}

	for _, alloc := range allocations {
		if !isClose(alloc.RelaySocket) {
			t.Error("Manager's allocations should be closed")
		}
	}
}

func randomFiveTuple() *FiveTuple {
	/* #nosec */
	return &FiveTuple{
		SrcAddr: &net.UDPAddr{IP: nil, Port: rand.Int()},
		DstAddr: &net.UDPAddr{IP: nil, Port: rand.Int()},
	}
}

func newTestManager() *Manager {
	loggerFactory := logging.NewDefaultLoggerFactory()
	config := &ManagerConfig{LeveledLogger: loggerFactory.NewLogger("test")}
	return NewManager(config)
}

func isClose(conn io.Closer) bool {
	closeErr := conn.Close()
	return closeErr != nil && strings.Contains(closeErr.Error(), "use of closed network connection")
}
