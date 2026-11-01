//go:build !windows

package main

import "errors"

var errNotWindows = errors.New("el proxy del sistema solo se configura en Windows")

func EnableSystemProxy(pacURL, pacBase string) (func(), error) { return nil, errNotWindows }
func DisableSystemProxy(pacBase string) (bool, error)          { return false, errNotWindows }
func DescribeSystemProxy() (string, error)                     { return "", errNotWindows }
