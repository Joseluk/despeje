//go:build windows

package main

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Configura el proxy de Windows (WinINet: Edge, Chrome, Firefox con "usar
// proxy del sistema"...) con INTERNET_OPTION_PER_CONNECTION_OPTION, como
// hace el panel de Configuración, para que todo quede coherente.

var (
	wininet      = windows.NewLazySystemDLL("wininet.dll")
	procSetOpt   = wininet.NewProc("InternetSetOptionW")
	procQueryOpt = wininet.NewProc("InternetQueryOptionW")
	procGlobFree = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
)

const (
	internetOptionRefresh         = 37
	internetOptionSettingsChanged = 39
	internetOptionPerConnection   = 75

	perConnFlags         = 1
	perConnProxyServer   = 2
	perConnProxyBypass   = 3
	perConnAutoConfigURL = 4

	proxyTypeDirect  = 0x1
	proxyTypeAutoURL = 0x4
)

// INTERNET_PER_CONN_OPTIONW: el union ocupa 8 bytes (FILETIME), alineado a
// puntero; uint64 en Go reproduce el layout en 386, amd64 y arm64.
type perConnOption struct {
	Option uint32
	Value  uint64
}

type perConnOptionList struct {
	Size        uint32
	Connection  *uint16
	OptionCount uint32
	OptionError uint32
	Options     *perConnOption
}

type proxyState struct {
	Flags         uint32
	Server        string
	Bypass        string
	AutoConfigURL string
}

func (o *perConnOption) str() **uint16 { return (**uint16)(unsafe.Pointer(&o.Value)) }

func queryProxy() (proxyState, error) {
	opts := []perConnOption{{Option: perConnFlags}, {Option: perConnProxyServer}, {Option: perConnProxyBypass}, {Option: perConnAutoConfigURL}}
	list := perConnOptionList{OptionCount: uint32(len(opts)), Options: &opts[0]}
	list.Size = uint32(unsafe.Sizeof(list))
	size := list.Size
	r, _, err := procQueryOpt.Call(0, internetOptionPerConnection, uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return proxyState{}, fmt.Errorf("InternetQueryOption: %w", err)
	}
	read := func(o *perConnOption) string {
		p := *o.str()
		if p == nil {
			return ""
		}
		s := windows.UTF16PtrToString(p)
		procGlobFree.Call(uintptr(unsafe.Pointer(p)))
		return s
	}
	return proxyState{
		Flags:         uint32(opts[0].Value),
		Server:        read(&opts[1]),
		Bypass:        read(&opts[2]),
		AutoConfigURL: read(&opts[3]),
	}, nil
}

func setProxy(st proxyState) error {
	opts := []perConnOption{{Option: perConnFlags, Value: uint64(st.Flags)}}
	var keep []*uint16
	for _, kv := range []struct {
		opt uint32
		val string
	}{{perConnProxyServer, st.Server}, {perConnProxyBypass, st.Bypass}, {perConnAutoConfigURL, st.AutoConfigURL}} {
		p, err := windows.UTF16PtrFromString(kv.val)
		if err != nil {
			return err
		}
		keep = append(keep, p)
		o := perConnOption{Option: kv.opt}
		*o.str() = p
		opts = append(opts, o)
	}
	list := perConnOptionList{OptionCount: uint32(len(opts)), Options: &opts[0]}
	list.Size = uint32(unsafe.Sizeof(list))
	r, _, err := procSetOpt.Call(0, internetOptionPerConnection, uintptr(unsafe.Pointer(&list)), uintptr(list.Size))
	runtime.KeepAlive(keep)
	runtime.KeepAlive(opts)
	if r == 0 {
		return fmt.Errorf("InternetSetOption: %w", err)
	}
	procSetOpt.Call(0, internetOptionSettingsChanged, 0, 0)
	procSetOpt.Call(0, internetOptionRefresh, 0, 0)
	return nil
}

// EnableSystemProxy apunta Windows al PAC de despeje si no lo está ya.
// Devuelve el PAC ajeno que había antes (si lo había) y si ha cambiado algo.
//
// Se escribe una sola vez y se deja puesto: el PAC termina en DIRECT, así que
// con despeje parado se navega igual. Reescribir el proxy en cada arranque
// desde un proceso en segundo plano es justo lo que hacen los troyanos
// bancarios, y Defender lo trata como tal.
func EnableSystemProxy(pacURL string) (replaced string, changed bool, err error) {
	prev, err := queryProxy()
	if err != nil {
		return "", false, err
	}
	if prev.AutoConfigURL == pacURL && prev.Flags&proxyTypeAutoURL != 0 {
		return "", false, nil
	}
	if prev.AutoConfigURL != "" && !strings.HasPrefix(prev.AutoConfigURL, pacURL) {
		replaced = prev.AutoConfigURL
	}
	next := prev
	next.Flags |= proxyTypeAutoURL | proxyTypeDirect
	next.AutoConfigURL = pacURL
	return replaced, true, setProxy(next)
}

// DisableSystemProxy quita el PAC de despeje si es el que está puesto.
func DisableSystemProxy(pacBase string) (bool, error) {
	st, err := queryProxy()
	if err != nil || !strings.HasPrefix(st.AutoConfigURL, pacBase) {
		return false, err
	}
	st.AutoConfigURL = ""
	st.Flags &^= proxyTypeAutoURL
	st.Flags |= proxyTypeDirect
	return true, setProxy(st)
}

func DescribeSystemProxy() (string, error) {
	st, err := queryProxy()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("flags=%#x pac=%q proxy=%q bypass=%q", st.Flags, st.AutoConfigURL, st.Server, st.Bypass), nil
}
