package main

import (
	"context"
	"testing"

	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/connectiondetails"
	"github.com/k8snetworkplumbingwg/ib-sriov-cni/pkg/guidcache"
)

func TestInvalidRetainedGUIDCannotPoisonLaterPublication(t *testing.T) {
	for _, bad := range []string{"00:00:00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff:ff:ff"} {
		t.Run(bad, func(t *testing.T) {
			cache := guidcache.New()
			f, args := newAnnotationTest(t)
			want := annotationEntry("net-a", "0000:03:00.0", 0)
			f.pod.Annotations[connectiondetails.StatusAnnotation] = `{"` + want.Key() + `":{"Status":"Ready","pfGuid":"` + bad + `"}}`
			reads := 0
			resolver := func(deviceID, retained string) (int, string, error) {
				return cachedPFDetail(context.Background(), cache, deviceID, retained,
					func(string) (int, error) { return 0, nil },
					func(string, string) (int, string, error) { reads++; return 0, want.PfGUID, nil })
			}
			_, firstErr := publishPodConnectionDetails(t.Context(), f, args, want.DeviceID, resolver)
			delete(f.pod.Annotations, connectiondetails.StatusAnnotation)
			_, nextErr := publishPodConnectionDetails(t.Context(), f, args, want.DeviceID, resolver)
			if firstErr != nil || nextErr != nil || reads != 1 {
				cached, _ := cache.Lookup(want.DeviceID)
				t.Fatalf("first ADD error=%v; after removing status error=%v; reads=%d cached=%q", firstErr, nextErr, reads, cached)
			}
		})
	}
}
