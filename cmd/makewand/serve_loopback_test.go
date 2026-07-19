package main

import "testing"

func TestIsLoopbackAddr_Table(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"localhost:8080", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"127.0.0.53:53", true},
		{"127.1.2.3:9090", true},
		{":8080", false},        // bind-all with port
		{"", false},             // empty
		{"0.0.0.0:8080", false}, // explicit any-IPv4
		{"[::]:8080", false},    // any-IPv6
		{"192.168.1.10:8080", false},
		{"10.0.0.5:8080", false},
		{"example.com:8080", false},
		{"8.8.8.8:443", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}
