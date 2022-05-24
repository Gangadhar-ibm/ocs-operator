package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/red-hat-storage/ocs-operator/metrics/internal/options"
	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	rookclient "github.com/rook/rook/pkg/client/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

type RBDMirrorPoolStatusVerbose struct {
	PoolName      string
	PoolNamespace string
	MirrorStatus  RBDMirrorStatusVerbose
}
type RBDMirrorStatusVerbose struct {
	Summary RBDMirrorPoolStatusSummary `json:"summary"`
	Daemons []RBDMirrorDaemonStatus    `json:"daemons"`
	Images  []RBDMirrorImageStatus     `json:"images"`
}

type RBDMirrorPoolStatusSummary struct {
	Health       string                     `json:"health"`
	DaemonHealth string                     `json:"daemon_health"`
	ImageHealth  string                     `json:"image_health"`
	States       RBDMirrorImageStatusStates `json:"states"`
}

type RBDMirrorImageStatusStates struct {
	Unknown        int `json:"unknown"`
	Error          int `json:"error"`
	Syncing        int `json:"syncing"`
	StartingReplay int `json:"starting_replay"`
	Replaying      int `json:"replaying"`
	StoppingReplay int `json:"stopping_replay"`
	Stopped        int `json:"stopped"`
}

type RBDMirrorDaemonStatus struct {
	ServiceID   string `json:"service_id"`
	InstanceID  string `json:"instance_id"`
	ClientID    string `json:"client_id"`
	Hostname    string `json:"hostname"`
	CephVersion string `json:"ceph_version"`
	Leader      bool   `json:"leader"`
	Health      string `json:"health"`
}

type RBDMirrorImageStatus struct {
	Name          string                 `json:"name"`
	GlobalID      string                 `json:"global_id"`
	State         string                 `json:"state"`
	Description   string                 `json:"description"`
	DaemonService RBDMirrorDaemonService `json:"daemon_service"`
	LastUpdate    string                 `json:"last_update"`
	PeerSites     []RBDMirrorPeerSite    `json:"peer_sites"`
}

type RBDMirrorDaemonService struct {
	ServiceID  string `json:"service_id"`
	InstanceID string `json:"instance_id"`
	DaemonID   string `json:"daemon_id"`
	Hostname   string `json:"hostname"`
}

type RBDMirrorPeerSite struct {
	SiteName    string `json:"site_name"`
	MirrorUuids string `json:"mirror_uuids"`
	State       string `json:"state"`
	Description string `json:"description"`
	LastUpdate  string `json:"last_update"`
}

type RBDMirrorPeerSiteDescription struct {
	BytesPerSecond          float64 `json:"bytes_per_second"`
	BytesPerSnapshot        float64 `json:"bytes_per_snapshot"`
	LocalSnapshotTimestamp  int64   `json:"local_snapshot_timestamp"`
	RemoteSnapshotTimestamp int64   `json:"remote_snapshot_timestamp"`
	ReplayState             string  `json:"replay_state"`
}

const (
	cephConfigRoot = "/etc/ceph"
	cephConfigPath = "/etc/ceph/ceph.conf"
	keyRing        = "/etc/ceph/keyring"
)

var cephConfig = []byte(`[global]
auth_cluster_required = cephx
auth_service_required = cephx
auth_client_required = cephx
`)

type csiClusterConfig struct {
	ClusterID string   `json:"clusterID"`
	Monitors  []string `json:"monitors"`
}

// Cache mirror data for all CephBlockPools with mirroring enabled

var _ cache.Store = &RBDMirrorStore{}

// RBDMirrorStore implements the k8s.io/client-go/tools/cache.Store
// interface. It stores rbd mirror data.
type RBDMirrorStore struct {
	Mutex sync.RWMutex
	// Store is a map of Pool UID to RBDMirrorPoolStatusVerbose
	Store map[types.UID]RBDMirrorPoolStatusVerbose
	// rbdCommandInput is a struct that contains the input for the rbd command
	// for each AllowdNamespaces
	rbdCommandInput   map[string]*rbdCommandInput
	kubeclient        clientset.Interface
	allowedNamespaces []string
}

func NewRBDMirrorStore(opts *options.Options) *RBDMirrorStore {
	// write Ceph config file before issuing RBD mirror commands
	err := writeCephConfig()
	if err != nil {
		// With the current implementation, this is not possible.
		panic(err)
	}
	return &RBDMirrorStore{
		Store:             map[types.UID]RBDMirrorPoolStatusVerbose{},
		rbdCommandInput:   map[string]*rbdCommandInput{},
		kubeclient:        clientset.NewForConfigOrDie(opts.Kubeconfig),
		allowedNamespaces: opts.AllowedNamespaces,
	}
}

func (s *RBDMirrorStore) WithRBDCommandInput(namespace string) error {
	var allow bool
	for _, item := range s.allowedNamespaces {
		if item == namespace {
			allow = true
			break
		}
	}
	if !allow {
		return fmt.Errorf("rbd-mirror metrics collection from namespace %q is not allowed", namespace)
	}

	secret, err := s.kubeclient.CoreV1().Secrets(namespace).Get(context.TODO(), "rook-ceph-mon", v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret in namespace %q: %v", namespace, err)
	}
	key, ok := secret.Data["ceph-secret"]
	if !ok {
		return fmt.Errorf("failed to get client key from secret in namespace %q", namespace)
	}
	id := "admin"

	configmap, err := s.kubeclient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), "rook-ceph-csi-config", v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get configmap in namespace %q: %v", namespace, err)
	}

	data, ok := configmap.Data["csi-cluster-config-json"]
	if !ok {
		return fmt.Errorf("failed to get CSI cluster config from configmap in namespace %q", namespace)
	}

	var clusterConfig []csiClusterConfig
	err = json.Unmarshal([]byte(data), &clusterConfig)
	if err != nil {
		return fmt.Errorf("failed to unmarshal csi-cluster-config-json in namespace %q: %v", namespace, err)
	}

	if len(clusterConfig) == 0 {
		return fmt.Errorf("expected 1 or more CSI cluster config but found 0 from configmap in namespace %q", namespace)
	}
	if len(clusterConfig[0].Monitors) == 0 {
		return fmt.Errorf("expected 1 or more monitors but found 0 from configmap in namespace %q", namespace)
	}

	input := rbdCommandInput{}
	input.monitor = clusterConfig[0].Monitors[0]
	input.id = id
	input.key = string(key)
	s.rbdCommandInput[namespace] = &input

	return nil
}

func (s *RBDMirrorStore) Add(obj interface{}) error {
	o, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	pool, ok := obj.(*cephv1.CephBlockPool)
	if !ok {
		return fmt.Errorf("unexpected object of type %T", obj)
	}

	if !pool.Spec.Mirroring.Enabled {
		klog.Infof("skipping rbd mirror status update for pool %s/%s because mirroring is disabled", pool.Namespace, pool.Name)
		return nil
	}

	if _, ok := s.rbdCommandInput[pool.Namespace]; !ok {
		err := s.WithRBDCommandInput(pool.Namespace)
		if err != nil {
			klog.Errorf("Failed to initialize rbd command input for pool %s/%s: %v", pool.Namespace, pool.Name, err)
			return fmt.Errorf("rbd command error for pool %s/%s : %v", pool.Namespace, pool.Name, err)
		}
	}

	mirrorStatus, err := s.rbdCommandInput[pool.Namespace].rbdImageStatus(pool.Name)
	if err != nil {
		return fmt.Errorf("rbd command error: %v", err)
	}

	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	s.Store[o.GetUID()] = RBDMirrorPoolStatusVerbose{
		PoolName:      pool.Name,
		PoolNamespace: pool.Namespace,
		MirrorStatus:  mirrorStatus,
	}

	return nil
}

func (s *RBDMirrorStore) Update(obj interface{}) error {
	return s.Add(obj)
}

func (s *RBDMirrorStore) Delete(obj interface{}) error {
	o, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	delete(s.Store, o.GetUID())

	return nil
}

func (s *RBDMirrorStore) List() []interface{} {
	return nil
}

func (s *RBDMirrorStore) ListKeys() []string {
	return nil
}

func (s *RBDMirrorStore) Get(obj interface{}) (item interface{}, exists bool, err error) {
	return nil, false, nil
}

func (s *RBDMirrorStore) GetByKey(key string) (item interface{}, exists bool, err error) {
	return nil, false, nil
}

func (s *RBDMirrorStore) Replace(list []interface{}, _ string) error {
	s.Mutex.Lock()
	s.Store = map[types.UID]RBDMirrorPoolStatusVerbose{}
	s.Mutex.Unlock()

	for _, o := range list {
		err := s.Add(o)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *RBDMirrorStore) Resync() error {
	klog.Infof("RBD mirror store resync started at %v", time.Now())
	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	for poolUUID, poolStatusVerbose := range s.Store {
		if _, ok := s.rbdCommandInput[poolStatusVerbose.PoolNamespace]; !ok {
			err := s.WithRBDCommandInput(poolStatusVerbose.PoolNamespace)
			if err != nil {
				klog.Errorf("Failed to initialize rbd command input for pool %s/%s: %v", poolStatusVerbose.PoolNamespace, poolStatusVerbose.PoolName, err)
				continue
			}
		}

		mirrorStatus, err := s.rbdCommandInput[poolStatusVerbose.PoolNamespace].rbdImageStatus(poolStatusVerbose.PoolName)
		if err != nil {
			klog.Errorf("rbd command error: %v", err)
			continue
		}

		s.Store[poolUUID] = RBDMirrorPoolStatusVerbose{
			PoolName:      poolStatusVerbose.PoolName,
			PoolNamespace: poolStatusVerbose.PoolNamespace,
			MirrorStatus:  mirrorStatus,
		}
	}
	klog.Infof("RBD mirror store resync ended at %v", time.Now())
	return nil
}

func CreateCephBlockPoolListWatch(cephClient rookclient.Interface, namespace, fieldSelector string) cache.ListerWatcher {
	return &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = fieldSelector
			return cephClient.CephV1().CephBlockPools(namespace).List(context.TODO(), opts)
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = fieldSelector
			return cephClient.CephV1().CephBlockPools(namespace).Watch(context.TODO(), opts)
		},
	}
}

/* RBD CLI Commands */

type rbdCommandInput struct {
	monitor, id, key string
}

func (in *rbdCommandInput) rbdImageStatus(poolName string) (RBDMirrorStatusVerbose, error) {
	var cmd []byte
	var rbdMirrorStatusVerbose RBDMirrorStatusVerbose

	if in.monitor == "" && in.id == "" && in.key == "" {
		return rbdMirrorStatusVerbose, errors.New("unable to get RBD mirror data. RBD command input not specified")
	}

	args := []string{"mirror", "pool", "status", poolName, "--verbose", "--format", "json", "-m", in.monitor, "--id", in.id, "--key", in.key, "--debug-rbd", "0"}
	cmd, err := execCommand("rbd", args)
	if err != nil {
		return rbdMirrorStatusVerbose, err
	}

	err = json.Unmarshal(cmd, &rbdMirrorStatusVerbose)

	return rbdMirrorStatusVerbose, err
}

func execCommand(command string, args []string) ([]byte, error) {
	cmd := exec.Command(command, args...)
	return cmd.CombinedOutput()
}

/*
	Copied from https://github.com/ceph/ceph-csi/blob/70fc6db2cfe3f00945c030f0d7f83ea1e2d21a00/internal/util/cephconf.go
	Functions to create ceph.conf and keyring files internally.
*/

func createCephConfigRoot() error {
	return os.MkdirAll(cephConfigRoot, 0o755)
}

// createKeyRingFile creates the keyring files to fix above error message logging.
func createKeyRingFile() error {
	var err error
	if _, err = os.Stat(keyRing); os.IsNotExist(err) {
		_, err = os.Create(keyRing)
	}

	return err
}

// writeCephConfig writes out a basic ceph.conf file, making it easy to use
// ceph related CLIs.
func writeCephConfig() error {
	var err error
	if err = createCephConfigRoot(); err != nil {
		return err
	}

	if _, err = os.Stat(cephConfigPath); os.IsNotExist(err) {
		err = os.WriteFile(cephConfigPath, cephConfig, 0o600)
	}

	if err != nil {
		return err
	}

	return createKeyRingFile()
}
