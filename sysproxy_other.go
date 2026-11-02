//go:build !windows

package main

import "errors"

var errNotWindows = errors.New("el proxy del sistema solo se configura en Windows")

func EnableSystemProxy(pacURL string) (string, bool, error) { return "", false, errNotWindows }
func DisableSystemProxy(pacBase string) (bool, error)       { return false, errNotWindows }
func DescribeSystemProxy() (string, error)                  { return "", errNotWindows }
