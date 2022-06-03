/*
Copyright 2016 The Kubernetes Authors.
Modifications Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dynamic

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kcp-dev/logicalcluster"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/streaming"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	restclient "k8s.io/client-go/rest"
	restclientwatch "k8s.io/client-go/rest/watch"
)

// All tests here borrow heavily from
// https://github.com/kubernetes/kubernetes/blob/d126b1483840b5ea7c0891d3e7a693bd50fae7f8/staging/src/k8s.io/client-go/dynamic/client_test.go

func getJSON(version, kind, name string) []byte {
	return []byte(fmt.Sprintf(`{"apiVersion": %q, "kind": %q, "metadata": {"name": %q}}`, version, kind, name))
}

func getListJSON(version, kind string, items ...[]byte) []byte {
	json := fmt.Sprintf(`{"apiVersion": %q, "kind": %q, "items": [%s]}`,
		version, kind, bytes.Join(items, []byte(",")))
	return []byte(json)
}

func getObject(version, kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": version,
			"kind":       kind,
			"metadata": map[string]interface{}{
				"name": name,
			},
		},
	}
}

func getClientServer(h func(http.ResponseWriter, *http.Request)) (*ClusterDynamicClient, *httptest.Server, error) {
	srv := httptest.NewServer(http.HandlerFunc(h))
	cl, err := NewClusterDynamicClientForConfig(&restclient.Config{
		Host: srv.URL,
	})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return cl, srv, nil
}

func TestList(t *testing.T) {
	tcs := map[string]struct {
		name      string
		namespace string
		cluster   logicalcluster.Name
		path      string
		resp      []byte
		want      *unstructured.UnstructuredList
	}{
		"wildcard list": {
			name:    "cluster_list",
			cluster: logicalcluster.Wildcard,
			path:    "/clusters/*/apis/gtest/vtest/rtest",
			resp: getListJSON("vTest", "rTestList",
				getJSON("vTest", "rTest", "item1"),
				getJSON("vTest", "rTest", "item2")),
			want: &unstructured.UnstructuredList{
				Object: map[string]interface{}{
					"apiVersion": "vTest",
					"kind":       "rTestList",
				},
				Items: []unstructured.Unstructured{
					*getObject("vTest", "rTest", "item1"),
					*getObject("vTest", "rTest", "item2"),
				},
			},
		},
		"cluster-scoped list": {
			name:    "cluster_list",
			cluster: logicalcluster.New("testcluster"),
			path:    "/clusters/testcluster/apis/gtest/vtest/rtest",
			resp: getListJSON("vTest", "rTestList",
				getJSON("vTest", "rTest", "item1"),
				getJSON("vTest", "rTest", "item2")),
			want: &unstructured.UnstructuredList{
				Object: map[string]interface{}{
					"apiVersion": "vTest",
					"kind":       "rTestList",
				},
				Items: []unstructured.Unstructured{
					*getObject("vTest", "rTest", "item1"),
					*getObject("vTest", "rTest", "item2"),
				},
			},
		},
		"cluster + namespace scoped list": {
			name:      "namespaced_list",
			namespace: "nstest",
			cluster:   logicalcluster.New("clustertest"),
			path:      "/clusters/clustertest/apis/gtest/vtest/namespaces/nstest/rtest",
			resp: getListJSON("vTest", "rTestList",
				getJSON("vTest", "rTest", "item1"),
				getJSON("vTest", "rTest", "item2")),
			want: &unstructured.UnstructuredList{
				Object: map[string]interface{}{
					"apiVersion": "vTest",
					"kind":       "rTestList",
				},
				Items: []unstructured.Unstructured{
					*getObject("vTest", "rTest", "item1"),
					*getObject("vTest", "rTest", "item2"),
				},
			},
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: "rtest"}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("List(%q) got HTTP method %s. wanted GET", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("List(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}

			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			_, err := w.Write(tc.resp)
			if err != nil {
				t.Errorf("unexpected error when writing response: %v", err)
			}

		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		got, err := cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			t.Errorf("unexpected error when listing %q: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("List(%q) want: %v\ngot: %v", tc.name, tc.want, got)
		}
	}
}

func TestWatch(t *testing.T) {
	tcs := map[string]struct {
		name      string
		namespace string
		cluster   logicalcluster.Name
		events    []watch.Event
		path      string
		query     string
	}{
		"wildcard watch": {
			name:    "normal_watch",
			path:    "/clusters/*/apis/gtest/vtest/rtest",
			cluster: logicalcluster.Wildcard,
			query:   "watch=true",
			events: []watch.Event{
				{Type: watch.Added, Object: getObject("gtest/vTest", "rTest", "normal_watch")},
				{Type: watch.Modified, Object: getObject("gtest/vTest", "rTest", "normal_watch")},
				{Type: watch.Deleted, Object: getObject("gtest/vTest", "rTest", "normal_watch")},
			},
		},
		"cluster scoped watch": {
			name:    "normal_watch",
			path:    "/clusters/ctest/apis/gtest/vtest/rtest",
			cluster: logicalcluster.New("ctest"),
			query:   "watch=true",
			events: []watch.Event{
				{Type: watch.Added, Object: getObject("gtest/vTest", "rTest", "normal_watch")},
				{Type: watch.Modified, Object: getObject("gtest/vTest", "rTest", "normal_watch")},
				{Type: watch.Deleted, Object: getObject("gtest/vTest", "rTest", "normal_watch")},
			},
		},
		"cluster and namespace scoped watch": {
			name:      "namespaced_watch",
			namespace: "nstest",
			path:      "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest",
			cluster:   logicalcluster.New("ctest"),
			query:     "watch=true",
			events: []watch.Event{
				{Type: watch.Added, Object: getObject("gtest/vTest", "rTest", "namespaced_watch")},
				{Type: watch.Modified, Object: getObject("gtest/vTest", "rTest", "namespaced_watch")},
				{Type: watch.Deleted, Object: getObject("gtest/vTest", "rTest", "namespaced_watch")},
			},
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: "rtest"}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("Watch(%q) got HTTP method %s. wanted GET", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("Watch(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}
			if r.URL.RawQuery != tc.query {
				t.Errorf("Watch(%q) got query %s. wanted %s", tc.name, r.URL.RawQuery, tc.query)
			}

			w.Header().Set("Content-Type", "application/json")

			enc := restclientwatch.NewEncoder(streaming.NewEncoder(w, unstructured.UnstructuredJSONScheme), unstructured.UnstructuredJSONScheme)
			for _, e := range tc.events {
				err := enc.Encode(&e)
				if err != nil {
					t.Errorf("Unexpected error encoding event: %v", err)
				}
			}
		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		watcher, err := cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).Watch(context.TODO(), metav1.ListOptions{})
		if err != nil {
			t.Errorf("unexpected error when watching %q: %v", tc.name, err)
			continue
		}

		for _, want := range tc.events {
			got := <-watcher.ResultChan()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Watch(%q) want: %v\ngot: %v", tc.name, want, got)
			}
		}
	}
}

func TestGet(t *testing.T) {
	tcs := map[string]struct {
		resource    string
		subresource []string
		cluster     logicalcluster.Name
		namespace   string
		name        string
		path        string
		resp        []byte
		want        *unstructured.Unstructured
	}{
		"cluster-scoped get": {
			resource: "rtest",
			name:     "normal_get",
			cluster:  logicalcluster.New("test"),
			path:     "/clusters/test/apis/gtest/vtest/rtest/normal_get",
			resp:     getJSON("vTest", "rTest", "normal_get"),
			want:     getObject("vTest", "rTest", "normal_get"),
		},
		"cluster and namespace scoped get": {
			resource:  "rtest",
			namespace: "nstest",
			cluster:   logicalcluster.New("test"),
			name:      "namespaced_get",
			path:      "/clusters/test/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_get",
			resp:      getJSON("vTest", "rTest", "namespaced_get"),
			want:      getObject("vTest", "rTest", "namespaced_get"),
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: tc.resource}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				t.Errorf("Get(%q) got HTTP method %s. wanted GET", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("Get(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}

			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			_, err := w.Write(tc.resp)
			if err != nil {
				t.Errorf("Unexpected error writing response: %v", err)
			}
		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		got, err := cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).Get(context.TODO(), tc.name, metav1.GetOptions{}, tc.subresource...)
		if err != nil {
			t.Errorf("unexpected error when getting %q: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Get(%q) want: %v\ngot: %v", tc.name, tc.want, got)
		}
	}
}

func TestDelete(t *testing.T) {
	background := metav1.DeletePropagationBackground
	uid := types.UID("uid")

	statusOK := &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status"},
		Status:   metav1.StatusSuccess,
	}
	tcs := map[string]struct {
		subresource   []string
		namespace     string
		cluster       logicalcluster.Name
		name          string
		path          string
		deleteOptions metav1.DeleteOptions
	}{
		"cluster scoped delete": {
			name:    "normal_delete",
			cluster: logicalcluster.New("ctest"),
			path:    "/clusters/ctest/apis/gtest/vtest/rtest/normal_delete",
		},
		"cluster + namespace scoped delete": {
			namespace: "nstest",
			name:      "namespaced_delete",
			cluster:   logicalcluster.New("test"),
			path:      "/clusters/test/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_delete",
		},
		"cluster scoped subresource delete": {
			subresource: []string{"srtest"},
			name:        "normal_delete",
			cluster:     logicalcluster.New("test"),
			path:        "/clusters/test/apis/gtest/vtest/rtest/normal_delete/srtest",
		},
		"cluster and namespace scoped subresource delete": {
			subresource: []string{"srtest"},
			namespace:   "nstest",
			name:        "namespaced_delete",
			cluster:     logicalcluster.New("test"),
			path:        "/clusters/test/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_delete/srtest",
		},
		"cluster and namespace scoped delete with options": {
			namespace:     "nstest",
			name:          "namespaced_delete_with_options",
			path:          "/clusters/test/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_delete_with_options",
			cluster:       logicalcluster.New("test"),
			deleteOptions: metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &background},
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: "rtest"}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "DELETE" {
				t.Errorf("Delete(%q) got HTTP method %s. wanted DELETE", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("Delete(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}

			content := r.Header.Get("Content-Type")
			if content != runtime.ContentTypeJSON {
				t.Errorf("Delete(%q) got Content-Type %s. wanted %s", tc.name, content, runtime.ContentTypeJSON)
			}

			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			err := unstructured.UnstructuredJSONScheme.Encode(statusOK, w)
			if err != nil {
				t.Errorf("Unexpected error writing response: %v", err)
			}
		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		err = cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).Delete(context.TODO(), tc.name, tc.deleteOptions, tc.subresource...)
		if err != nil {
			t.Errorf("unexpected error when deleting %q: %v", tc.name, err)
			continue
		}
	}
}

func TestCreate(t *testing.T) {
	tcs := map[string]struct {
		resource    string
		subresource []string
		name        string
		namespace   string
		cluster     logicalcluster.Name
		obj         *unstructured.Unstructured
		path        string
	}{
		"cluster scoped create": {
			resource: "rtest",
			name:     "normal_create",
			path:     "/clusters/ctest/apis/gtest/vtest/rtest",
			cluster:  logicalcluster.New("ctest"),
			obj:      getObject("gtest/vTest", "rTest", "normal_create"),
		},
		"cluster and namespace scoped create": {
			resource:  "rtest",
			name:      "namespaced_create",
			namespace: "nstest",
			cluster:   logicalcluster.New("ctest"),
			path:      "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest",
			obj:       getObject("gtest/vTest", "rTest", "namespaced_create"),
		},
		"cluster scoped subresource create": {
			resource:    "rtest",
			subresource: []string{"srtest"},
			name:        "normal_subresource_create",
			cluster:     logicalcluster.New("ctest"),
			path:        "/clusters/ctest/apis/gtest/vtest/rtest/normal_subresource_create/srtest",
			obj:         getObject("vTest", "srTest", "normal_subresource_create"),
		},
		"cluster and namespace scoped subresource create": {
			resource:    "rtest/",
			subresource: []string{"srtest"},
			name:        "namespaced_subresource_create",
			namespace:   "nstest",
			cluster:     logicalcluster.New("ctest"),
			path:        "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_subresource_create/srtest",
			obj:         getObject("vTest", "srTest", "namespaced_subresource_create"),
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: tc.resource}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Errorf("Create(%q) got HTTP method %s. wanted POST", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("Create(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}

			content := r.Header.Get("Content-Type")
			if content != runtime.ContentTypeJSON {
				t.Errorf("Create(%q) got Content-Type %s. wanted %s", tc.name, content, runtime.ContentTypeJSON)
			}

			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				t.Errorf("Create(%q) unexpected error reading body: %v", tc.name, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			_, err = w.Write(data)
			if err != nil {
				t.Errorf("Create(%q) unexpected error writing body: %v", tc.name, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		got, err := cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).Create(context.TODO(), tc.obj, metav1.CreateOptions{}, tc.subresource...)
		if err != nil {
			t.Errorf("unexpected error when creating %q: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(got, tc.obj) {
			t.Errorf("Create(%q) want: %v\ngot: %v", tc.name, tc.obj, got)
		}
	}
}

func TestUpdate(t *testing.T) {
	tcs := map[string]struct {
		resource    string
		subresource []string
		name        string
		namespace   string
		cluster     logicalcluster.Name
		obj         *unstructured.Unstructured
		path        string
	}{
		"cluster scoped update": {
			resource: "rtest",
			name:     "normal_update",
			cluster:  logicalcluster.New("ctest"),
			path:     "/clusters/ctest/apis/gtest/vtest/rtest/normal_update",
			obj:      getObject("gtest/vTest", "rTest", "normal_update"),
		},
		"cluster and namespace scoped update": {
			resource:  "rtest",
			name:      "namespaced_update",
			namespace: "nstest",
			cluster:   logicalcluster.New("ctest"),
			path:      "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_update",
			obj:       getObject("gtest/vTest", "rTest", "namespaced_update"),
		},
		"cluster scoped subresource update": {
			resource:    "rtest",
			subresource: []string{"srtest"},
			name:        "normal_subresource_update",
			path:        "/clusters/ctest/apis/gtest/vtest/rtest/normal_update/srtest",
			cluster:     logicalcluster.New("ctest"),
			obj:         getObject("gtest/vTest", "srTest", "normal_update"),
		},
		"cluster and namespace scoped subresource update": {
			resource:    "rtest",
			subresource: []string{"srtest"},
			name:        "namespaced_subresource_update",
			namespace:   "nstest",
			cluster:     logicalcluster.New("ctest"),
			path:        "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_update/srtest",
			obj:         getObject("gtest/vTest", "srTest", "namespaced_update"),
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: tc.resource}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PUT" {
				t.Errorf("Update(%q) got HTTP method %s. wanted PUT", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("Update(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}

			content := r.Header.Get("Content-Type")
			if content != runtime.ContentTypeJSON {
				t.Errorf("Uppdate(%q) got Content-Type %s. wanted %s", tc.name, content, runtime.ContentTypeJSON)
			}

			w.Header().Set("Content-Type", runtime.ContentTypeJSON)
			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				t.Errorf("Update(%q) unexpected error reading body: %v", tc.name, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			_, err = w.Write(data)
			if err != nil {
				t.Errorf("Unexpected error writing response: %v", err)
			}
		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		got, err := cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).Update(context.TODO(), tc.obj, metav1.UpdateOptions{}, tc.subresource...)
		if err != nil {
			t.Errorf("unexpected error when updating %q: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(got, tc.obj) {
			t.Errorf("Update(%q) want: %v\ngot: %v", tc.name, tc.obj, got)
		}
	}
}

func TestPatch(t *testing.T) {
	tcs := map[string]struct {
		resource    string
		subresource []string
		name        string
		namespace   string
		cluster     logicalcluster.Name
		patch       []byte
		want        *unstructured.Unstructured
		path        string
	}{
		"cluster scoped patch": {
			resource: "rtest",
			name:     "normal_patch",
			cluster:  logicalcluster.New("ctest"),
			path:     "/clusters/ctest/apis/gtest/vtest/rtest/normal_patch",
			patch:    getJSON("gtest/vTest", "rTest", "normal_patch"),
			want:     getObject("gtest/vTest", "rTest", "normal_patch"),
		},
		"cluster and namespace scoped patch": {
			resource:  "rtest",
			name:      "namespaced_patch",
			namespace: "nstest",
			cluster:   logicalcluster.New("ctest"),
			path:      "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_patch",
			patch:     getJSON("gtest/vTest", "rTest", "namespaced_patch"),
			want:      getObject("gtest/vTest", "rTest", "namespaced_patch"),
		},
		"cluster scoped subresource patch": {
			resource:    "rtest",
			subresource: []string{"srtest"},
			name:        "normal_subresource_patch",
			cluster:     logicalcluster.New("ctest"),
			path:        "/clusters/ctest/apis/gtest/vtest/rtest/normal_subresource_patch/srtest",
			patch:       getJSON("gtest/vTest", "srTest", "normal_subresource_patch"),
			want:        getObject("gtest/vTest", "srTest", "normal_subresource_patch"),
		},
		"cluster and namespace scoped subresource patch": {
			resource:    "rtest",
			subresource: []string{"srtest"},
			name:        "namespaced_subresource_patch",
			namespace:   "nstest",
			cluster:     logicalcluster.New("ctest"),
			path:        "/clusters/ctest/apis/gtest/vtest/namespaces/nstest/rtest/namespaced_subresource_patch/srtest",
			patch:       getJSON("gtest/vTest", "srTest", "namespaced_subresource_patch"),
			want:        getObject("gtest/vTest", "srTest", "namespaced_subresource_patch"),
		},
	}
	for _, tc := range tcs {
		resource := schema.GroupVersionResource{Group: "gtest", Version: "vtest", Resource: tc.resource}
		cl, srv, err := getClientServer(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PATCH" {
				t.Errorf("Patch(%q) got HTTP method %s. wanted PATCH", tc.name, r.Method)
			}

			if r.URL.Path != tc.path {
				t.Errorf("Patch(%q) got path %s. wanted %s", tc.name, r.URL.Path, tc.path)
			}

			content := r.Header.Get("Content-Type")
			if content != string(types.StrategicMergePatchType) {
				t.Errorf("Patch(%q) got Content-Type %s. wanted %s", tc.name, content, types.StrategicMergePatchType)
			}

			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				t.Errorf("Patch(%q) unexpected error reading body: %v", tc.name, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, err = w.Write(data)
			if err != nil {
				t.Errorf("unexpected error when writing response: %v", err)
			}
		})
		if err != nil {
			t.Errorf("unexpected error when creating client: %v", err)
			continue
		}
		defer srv.Close()

		got, err := cl.Cluster(tc.cluster).Resource(resource).Namespace(tc.namespace).Patch(context.TODO(), tc.name, types.StrategicMergePatchType, tc.patch, metav1.PatchOptions{}, tc.subresource...)
		if err != nil {
			t.Errorf("unexpected error when patching %q: %v", tc.name, err)
			continue
		}

		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Patch(%q) want: %v\ngot: %v", tc.name, tc.want, got)
		}
	}
}
