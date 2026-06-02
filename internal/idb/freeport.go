package idb

import "net"

// freePort asks the OS for an unused TCP port on the loopback interface.
// There is a small race between closing the listener and idb_companion
// binding the port; acceptable for the MVP.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
