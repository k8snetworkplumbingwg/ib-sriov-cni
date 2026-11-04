package main

import (
	"errors"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"

	localtypes "github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/types"
)

func TestParsePodIdentity(t *testing.T) {
	cases := map[string]struct{ namespace, name, uid string }{
		"IgnoreUnknown=1;K8S_POD_NAMESPACE=ns1;K8S_POD_NAME=vm-1;K8S_POD_UID=uid-1": {"ns1", "vm-1", "uid-1"},
		"K8S_POD_NAME=vm-1":         {"", "vm-1", ""},
		"K8S_POD_NAMESPACE=ns1":     {"ns1", "", ""},
		"":                          {"", "", ""},
		"malformed;also-no-equals":  {"", "", ""},
		"K8S_POD_NAME=vm=extra;X=1": {"", "vm=extra", ""},
	}
	for in, want := range cases {
		namespace, name, uid := parsePodIdentity(in)
		if namespace != want.namespace || name != want.name || uid != want.uid {
			t.Fatalf("parsePodIdentity(%q) = (%q, %q, %q), want %+v", in, namespace, name, uid, want)
		}
	}
}

func TestConfigureIBKubernetes(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		enabled, publish, vf, vfio, configured bool
		publishErr                             error
		wantPublish, wantErr                   bool
	}{
		{name: "disabled", publish: true, vfio: true},
		{name: "legacy gate", enabled: true, vfio: true, wantErr: true},
		{name: "legacy ready", enabled: true, vfio: true, configured: true},
		{name: "publish without ready", enabled: true, publish: true, vfio: true, wantPublish: true},
		{name: "publish already ready", enabled: true, publish: true, vfio: true, configured: true, wantPublish: true},
		{name: "publication failure", enabled: true, publish: true, vfio: true, publishErr: errors.New("denied"), wantPublish: true, wantErr: true},
		{name: "VF retains gate", enabled: true, publish: true, vf: true, vfio: true, wantErr: true},
		{name: "VF configured", enabled: true, publish: true, vf: true, vfio: true, configured: true},
		{name: "non VFIO retains gate", enabled: true, publish: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := &localtypes.NetConf{}
			conf.IBKubernetesEnabled, conf.PublishPfGUID = tc.enabled, tc.publish
			conf.IsVFDevice, conf.VfioPciMode = tc.vf, tc.vfio
			if tc.configured {
				conf.Args.CNI = map[string]string{infiniBandAnnotation: configuredInfiniBand}
			}
			called := false
			err := configureIBKubernetes(&skel.CmdArgs{}, conf, func(*skel.CmdArgs, *localtypes.NetConf) error {
				called = true
				return tc.publishErr
			})
			if called != tc.wantPublish || (err != nil) != tc.wantErr {
				t.Fatalf("called=%v err=%v, want called=%v error=%v", called, err, tc.wantPublish, tc.wantErr)
			}
			if tc.publishErr != nil && !errors.Is(err, tc.publishErr) {
				t.Fatalf("publication error lost: %v", err)
			}
		})
	}
}
