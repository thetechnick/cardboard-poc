package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cardboard.package-operator.run/internal/filewatch"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/mt-sre/devkube/dev"
	"github.com/pterm/pterm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	kindv1alpha4 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/yaml"
)

func main() {
	needsToWatch := flag.Bool("w", false, "")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// 1. Read config
	ctx := context.Background()
	// 2. Ensure Cluster
	spin, _ := pterm.DefaultSpinner.Start(
		"ensuring kind cluster...")
	env, err := envSetup(ctx)
	if err != nil {
		panic(err)
	}
	spin.Success()

	// Ensure cardboard task & workspace PV is setup
	spin, _ = pterm.DefaultSpinner.Start(
		"ensuring cardboard pipeline...")
	if err := ensureCardboard(ctx, env.Cluster.CtrlClient); err != nil {
		panic(err)
	}
	spin.Success()

	if err := do(ctx, env); err != nil {
		panic(err)
	}

	// Watcher?
	if needsToWatch != nil && *needsToWatch {
		watcher, err := filewatch.NewRecursiveWatcher()
		if err != nil {
			panic(err)
		}
		defer watcher.Close()

		if err := watcher.Add("."); err != nil {
			panic(err)
		}
		pterm.Info.Println("waiting for file changes...")

		q := filewatch.NewWorkQueue(watcher, func(ctx context.Context) error {
			if err := do(ctx, env); err != nil {
				return err
			}
			pterm.Info.Println("waiting for file changes...")
			return nil
		})
		if err := q.Run(ctx); err != nil {
			panic(err)
		}
	}
}

func do(ctx context.Context, env *dev.Environment) error {
	// 3. Build SRC Container
	// 4. Push SRC container
	spin, _ := pterm.DefaultSpinner.Start(
		"loading local source code into cluster...")
	if err := buildAndPush(); err != nil {
		return err
	}

	// 5. Load SRC container into PV
	if err := populateWorkspace(
		ctx, env.Cluster.CtrlClient, env.Cluster.Waiter); err != nil {
		return err
	}
	spin.Success()

	// 6. Update Pipeline
	spin, _ = pterm.DefaultSpinner.Start(
		"updating project pipelines...")
	if err := ensurePipeline(ctx, env.Cluster.CtrlClient); err != nil {
		return err
	}
	spin.Success()

	// 7. Create and wait for PipelineRun
	spin, _ = pterm.DefaultSpinner.Start(
		"running project pipelines...")
	if err := createPipelineRun(ctx, env.Cluster.CtrlClient, env.Cluster.Waiter); err != nil {
		return err
	}
	spin.Success()

	return nil
}

func createPipelineRun(
	ctx context.Context, c client.Client,
	w *dev.Waiter,
) error {
	objects, err := loadFromFile("config/run.yaml")
	if err != nil {
		return err
	}

	for _, obj := range objects {
		if err := c.Delete(ctx, obj.DeepCopy()); err != nil && !errors.IsNotFound(err) {
			return err
		}

		if err := c.Patch(
			ctx, obj.DeepCopy(), client.Apply,
			client.FieldOwner(fieldManager),
		); err != nil {
			return err
		}

		if err := w.WaitForCondition(
			ctx, &obj, "Succeeded", metav1.ConditionTrue,
		); err != nil {
			return err
		}
	}

	return nil
}

var splitDocumentRegex = regexp.MustCompile(`(?m)^---$`)

func ensurePipeline(ctx context.Context, c client.Client) error {
	objects, err := loadFromFile("config/pipeline.yaml")
	if err != nil {
		return err
	}

	for _, obj := range objects {
		if err := c.Patch(ctx, &obj, client.Apply, client.FieldOwner(fieldManager)); err != nil {
			return err
		}
	}
	return nil
}

const (
	fieldManager       = "cardboard.package-operator.run/cli"
	cardboardNamespace = "cardboard"
)

const sourceTaskRun = `
apiVersion: tekton.dev/v1beta1
kind: TaskRun
metadata:
  name: cardboard-populate-source
spec:
  workspaces:
  - name: output
    persistentVolumeClaim:
      claimName: cardboard-workdir
  params:
  - name: src-image
    value: localhost/src:v1
  taskRef:
    name: cardboard-get-source
`

func populateWorkspace(ctx context.Context, c client.Client, w *dev.Waiter) error {
	getSourceTaskRun := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(sourceTaskRun), getSourceTaskRun); err != nil {
		return err
	}
	getSourceTaskRun.SetNamespace(cardboardNamespace)

	if err := c.Delete(ctx, getSourceTaskRun); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting old TaskRun: %w", err)
	}

	if err := c.Patch(
		ctx, getSourceTaskRun, client.Apply, client.FieldOwner(fieldManager),
	); err != nil {
		return fmt.Errorf("ensuring Task: %w", err)
	}

	return w.WaitForCondition(
		ctx, getSourceTaskRun, "Succeeded", metav1.ConditionTrue,
	)
}

const sourceTask = `
apiVersion: tekton.dev/v1beta1
kind: Task
metadata:
  name: cardboard-get-source
spec:
  workspaces:
  - name: output
    description: The source will be copied onto the volume backing this Workspace.
  params:
  - name: src-image
    description: Source image to load from.
    type: string
  steps:
  - name: get-source
    image: $(params.src-image)
    workingDir: $(workspaces.output.path)
    script: |
      #!/bin/sh
      rm -rf .
      cp -a /source/* .
`

func ensureCardboard(ctx context.Context, c client.Client) error {
	cardboardNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: cardboardNamespace,
		},
	}
	cardboardNS.SetGroupVersionKind(schema.GroupVersionKind{
		Version: "v1",
		Kind:    "Namespace",
	})
	if err := c.Patch(
		ctx, cardboardNS, client.Apply, client.FieldOwner(fieldManager),
	); err != nil {
		return fmt.Errorf("ensuring Namespace: %w", err)
	}

	getSourceTask := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(sourceTask), getSourceTask); err != nil {
		return err
	}
	getSourceTask.SetNamespace(cardboardNS.Name)
	if err := c.Patch(
		ctx, getSourceTask, client.Apply, client.FieldOwner(fieldManager),
	); err != nil {
		return fmt.Errorf("ensuring Task: %w", err)
	}

	pvLabels := map[string]string{
		"type":                                  "local",
		"cardboard.package-operator.run/volume": "True",
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "cardboard-workdir",
			Labels: pvLabels,
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: "manual",
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/tmp/cardboard-workdir",
				},
			},
		},
	}
	pv.SetGroupVersionKind(schema.GroupVersionKind{
		Version: "v1",
		Kind:    "PersistentVolume",
	})
	if err := c.Patch(
		ctx, pv, client.Apply, client.FieldOwner(fieldManager),
	); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("ensuring PersistentVolume: %w", err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cardboard-workdir",
			Namespace: cardboardNS.Name,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("manual"),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: pvLabels,
			},
		},
	}
	pvc.SetGroupVersionKind(schema.GroupVersionKind{
		Version: "v1",
		Kind:    "PersistentVolumeClaim",
	})
	if err := c.Patch(
		ctx, pvc, client.Apply, client.FieldOwner(fieldManager),
	); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("ensuring PersistentVolumeClaim: %w", err)
	}

	return nil
}

const (
	clusterName        = "cardboard"
	localImageRegistry = "localhost"
)

func envSetup(ctx context.Context) (env *dev.Environment, err error) {
	env = dev.NewEnvironment(
		clusterName,
		filepath.Join(".cache", "dev-env"),
		dev.WithClusterOptions([]dev.ClusterOption{
			dev.WithWaitOptions([]dev.WaitOption{
				dev.WithTimeout(2 * time.Minute)}),
		}),
		dev.WithClusterInitializers{
			dev.ClusterLoadObjectsFromHttp{
				"https://storage.googleapis.com/tekton-releases/pipeline/latest/release.yaml",
			},
			dev.ClusterLoadObjectsFromFiles{
				"config/local-registry.yaml",
			},
		},
		dev.WithKindClusterConfig(kindv1alpha4.Cluster{
			ContainerdConfigPatches: []string{
				// Replace `imageRegistry` with our local dev-registry.
				fmt.Sprintf(`[plugins."io.containerd.grpc.v1.cri".registry.mirrors."%s"]
	endpoint = ["http://localhost:31320"]`, localImageRegistry),
			},
			Nodes: []kindv1alpha4.Node{
				{
					Role: kindv1alpha4.ControlPlaneRole,
					ExtraPortMappings: []kindv1alpha4.PortMapping{
						// Open port to enable connectivity with local registry.
						{
							ContainerPort: 5001,
							HostPort:      5001,
							ListenAddress: "127.0.0.1",
							Protocol:      "TCP",
						},
					},
				},
			},
		}),
	)
	return env, env.Init(ctx)
}

func buildAndPush() error {
	ctx := context.Background()

	fs, err := FS(ctx, os.DirFS("."))
	if err != nil {
		return err
	}

	image, err := image(fs)
	if err != nil {
		return err
	}

	// tags := []string{"localhost:5001/src:v1"}

	return crane.Push(image, "localhost:5001/src:v1", crane.Insecure)

	// m := map[string]v1.Image{}
	// for _, tag := range tags {
	// 	m[tag] = image
	// }
	// if err := crane.MultiSave(m, path); err != nil {
	// 	return fmt.Errorf("dump to %s: %w", path, err)
	// }

	// return nil
}

func image(files map[string][]byte) (v1.Image, error) {
	img, err := crane.Pull("docker.io/library/alpine:latest")
	if err != nil {
		return nil, err
	}

	layer, err := crane.Layer(files)
	if err != nil {
		return nil, err
	}

	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		return nil, fmt.Errorf("create image from layer: %w", err)
	}

	img, err = mutate.Canonical(img)
	if err != nil {
		return nil, err
	}
	return img, nil
}

func FS(ctx context.Context, src fs.FS) (map[string][]byte, error) {
	bundle := map[string][]byte{}
	walker := func(path string, entry fs.DirEntry, ioErr error) error {
		switch {
		case ioErr != nil:
			return fmt.Errorf("access file %s: %w", path, ioErr)

		case entry.Name() == ".":
			// continue at root

		case strings.HasPrefix(entry.Name(), "."):
			if entry.IsDir() {
				return filepath.SkipDir
			}

		case entry.IsDir():
			// no special handling for directories

		default:
			data, err := fs.ReadFile(src, path)
			if err != nil {
				return fmt.Errorf("read file %s: %w", path, err)
			}
			bundle[filepath.Join("source", path)] = data
		}

		return nil
	}

	if err := fs.WalkDir(src, ".", walker); err != nil {
		return nil, fmt.Errorf("walk source dir: %w", err)
	}

	return bundle, nil
}

func loadFromFile(file string) ([]unstructured.Unstructured, error) {
	pipelineYaml, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	var out []unstructured.Unstructured
	docs := splitDocumentRegex.Split(string(pipelineYaml), -1)
	for _, doc := range docs {
		obj := unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			return nil, err
		}
		obj.SetNamespace(cardboardNamespace)
		out = append(out, obj)
	}
	return out, nil
}

func ensureObj(
	ctx context.Context,
	c client.Client,
	obj client.Object,
) error {
	if err := c.Patch(
		ctx, obj, client.Apply, client.FieldOwner(fieldManager),
	); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("ensuring PersistentVolumeClaim: %w", err)
	}
	return nil
}
