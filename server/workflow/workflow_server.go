package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/argoproj/argo-workflows/v3/errors"
	"github.com/argoproj/argo-workflows/v3/persist/sqldb"
	workflowpkg "github.com/argoproj/argo-workflows/v3/pkg/apiclient/workflow"
	"github.com/argoproj/argo-workflows/v3/pkg/apis/workflow"
	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	"github.com/argoproj/argo-workflows/v3/server/auth"
	servertypes "github.com/argoproj/argo-workflows/v3/server/types"
	sutils "github.com/argoproj/argo-workflows/v3/server/utils"
	"github.com/argoproj/argo-workflows/v3/server/workflow/store"
	argoutil "github.com/argoproj/argo-workflows/v3/util"
	"github.com/argoproj/argo-workflows/v3/util/fields"
	"github.com/argoproj/argo-workflows/v3/util/instanceid"
	"github.com/argoproj/argo-workflows/v3/util/logging"
	"github.com/argoproj/argo-workflows/v3/util/logs"
	"github.com/argoproj/argo-workflows/v3/workflow/common"
	"github.com/argoproj/argo-workflows/v3/workflow/creator"
	"github.com/argoproj/argo-workflows/v3/workflow/hydrator"
	"github.com/argoproj/argo-workflows/v3/workflow/util"
	"github.com/argoproj/argo-workflows/v3/workflow/validate"
)

const (
	latestAlias                  = "@latest"
	reSyncDuration               = 20 * time.Minute
	workflowTemplateResyncPeriod = 20 * time.Minute
)

type workflowServer struct {
	instanceIDService     instanceid.Service
	offloadNodeStatusRepo sqldb.OffloadNodeStatusRepo
	hydrator              hydrator.Interface
	wfArchive             sqldb.WorkflowArchive
	wfLister              store.WorkflowLister
	wfReflector           *cache.Reflector
	wftmplStore           servertypes.WorkflowTemplateStore
	cwftmplStore          servertypes.ClusterWorkflowTemplateStore
	wfDefaults            *wfv1.Workflow
}

var _ workflowpkg.WorkflowServiceServer = &workflowServer{}

// NewWorkflowServer returns a new WorkflowServer
func NewWorkflowServer(ctx context.Context, instanceIDService instanceid.Service, offloadNodeStatusRepo sqldb.OffloadNodeStatusRepo, wfArchive sqldb.WorkflowArchive, wfClientSet versioned.Interface, wfLister store.WorkflowLister, wfStore store.WorkflowStore, wftmplStore servertypes.WorkflowTemplateStore, cwftmplStore servertypes.ClusterWorkflowTemplateStore, wfDefaults *wfv1.Workflow, namespace *string) *workflowServer {
	ws := &workflowServer{
		instanceIDService:     instanceIDService,
		offloadNodeStatusRepo: offloadNodeStatusRepo,
		hydrator:              hydrator.New(offloadNodeStatusRepo),
		wfArchive:             wfArchive,
		wfLister:              wfLister,
		wftmplStore:           wftmplStore,
		cwftmplStore:          cwftmplStore,
		wfDefaults:            wfDefaults,
	}
	if wfStore != nil && namespace != nil {
		lw := &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return wfClientSet.ArgoprojV1alpha1().Workflows(*namespace).List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return wfClientSet.ArgoprojV1alpha1().Workflows(*namespace).Watch(ctx, options)
			},
		}
		wfReflector := cache.NewReflector(lw, &wfv1.Workflow{}, wfStore, reSyncDuration)
		ws.wfReflector = wfReflector
	}
	return ws
}

func (s *workflowServer) Run(stopCh <-chan struct{}) {
	if s.wfReflector != nil {
		s.wfReflector.Run(stopCh)
	}
}

func (s *workflowServer) CreateWorkflow(ctx context.Context, req *workflowpkg.WorkflowCreateRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)

	if req.Workflow == nil {
		return nil, sutils.ToStatusError(fmt.Errorf("workflow body not specified"), codes.InvalidArgument)
	}

	if req.Workflow.Namespace == "" {
		req.Workflow.Namespace = req.Namespace
	}

	s.instanceIDService.Label(req.Workflow)
	creator.LabelCreator(ctx, req.Workflow)

	wftmplGetter := s.wftmplStore.Getter(ctx, req.Workflow.Namespace)
	cwftmplGetter := s.cwftmplStore.Getter(ctx)

	err := validate.ValidateWorkflow(ctx, wftmplGetter, cwftmplGetter, req.Workflow, s.wfDefaults, validate.ValidateOpts{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	// if we are doing a normal dryRun, just return the workflow un-altered
	if req.CreateOptions != nil && len(req.CreateOptions.DryRun) > 0 {
		return req.Workflow, nil
	}
	if req.ServerDryRun {
		workflow, err := util.CreateServerDryRun(ctx, req.Workflow, wfClient)
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.InvalidArgument)
		}
		return workflow, nil
	}

	wf, err := wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Create(ctx, req.Workflow, metav1.CreateOptions{})
	logger := logging.RequireLoggerFromContext(ctx)
	if err != nil {
		if apierr.IsServerTimeout(err) && req.Workflow.GenerateName != "" && req.Workflow.Name != "" {
			errWithHint := fmt.Errorf(`create request failed due to timeout, but it's possible that workflow "%s" already exists. Original error: %w`, req.Workflow.Name, err)
			logger.WithError(err).Error(ctx, errWithHint.Error())
			return nil, sutils.ToStatusError(errWithHint, codes.DeadlineExceeded)
		}
		logger.WithError(err).Error(ctx, "Create request failed")
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	return wf, nil
}

func (s *workflowServer) GetWorkflow(ctx context.Context, req *workflowpkg.WorkflowGetRequest) (*wfv1.Workflow, error) {
	wfGetOption := metav1.GetOptions{}
	if req.GetOptions != nil {
		wfGetOption = *req.GetOptions
	}
	wfClient := auth.GetWfClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, wfGetOption)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}
	cleaner := fields.NewCleaner(req.Fields)
	if !cleaner.WillExclude("status.nodes") {
		if err := s.hydrator.Hydrate(ctx, wf); err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
	}
	newWf := &wfv1.Workflow{}
	if ok, err := cleaner.Clean(wf, &newWf); err != nil {
		// should this be InvalidArgument?
		return nil, sutils.ToStatusError(fmt.Errorf("unable to CleanFields in request: %w", err), codes.Internal)
	} else if ok {
		return newWf, nil
	}
	return wf, nil
}

func (s *workflowServer) ListWorkflows(ctx context.Context, req *workflowpkg.WorkflowListRequest) (*wfv1.WorkflowList, error) {
	listOption := metav1.ListOptions{}
	if req.ListOptions != nil {
		listOption = *req.ListOptions
	}
	s.instanceIDService.With(&listOption)

	options, err := sutils.BuildListOptions(listOption, req.Namespace, "", req.NameFilter, req.CreatedAfter, req.FinishedBefore)

	if err != nil {
		return nil, err
	}

	// verify if we have permission to list Workflows
	allowed, err := auth.CanI(ctx, "list", workflow.WorkflowPlural, options.Namespace, "")
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	if !allowed {
		return nil, status.Error(codes.PermissionDenied, fmt.Sprintf("Permission denied, you are not allowed to list workflows in namespace \"%s\". Maybe you want to specify a namespace with query parameter `.namespace=%s`?", options.Namespace, options.Namespace))
	}

	var wfs wfv1.Workflows
	liveWfCount, err := s.wfLister.CountWorkflows(ctx, req.Namespace, req.NameFilter, req.CreatedAfter, req.FinishedBefore, listOption)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	archivedCount, err := s.wfArchive.CountWorkflows(ctx, options)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	totalCount := liveWfCount + archivedCount

	// first fetch live workflows
	liveWfList := &wfv1.WorkflowList{}
	if liveWfCount > 0 && (options.Limit == 0 || options.Offset < int(liveWfCount)) {
		liveWfList, err = s.wfLister.ListWorkflows(ctx, req.Namespace, req.NameFilter, req.CreatedAfter, req.FinishedBefore, listOption)
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
		wfs = append(wfs, liveWfList.Items...)
	}

	// then fetch archived workflows
	if options.Limit == 0 ||
		int64(options.Offset+options.Limit) > liveWfCount {
		archivedOffset := options.Offset - int(liveWfCount)
		archivedLimit := options.Limit
		if archivedOffset < 0 {
			archivedOffset = 0
			archivedLimit = options.Limit - len(liveWfList.Items)
		}
		archivedWfList, err := s.wfArchive.ListWorkflows(ctx, options.WithLimit(archivedLimit).WithOffset(archivedOffset))
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
		wfs = append(wfs, archivedWfList...)
	}
	meta := metav1.ListMeta{ResourceVersion: liveWfList.ResourceVersion}
	if s.wfReflector != nil {
		meta.ResourceVersion = s.wfReflector.LastSyncResourceVersion()
	}
	remainCount := totalCount - int64(options.Offset) - int64(len(wfs))
	if remainCount < 0 {
		remainCount = 0
	}
	if remainCount > 0 {
		meta.Continue = fmt.Sprintf("%v", options.Offset+len(wfs))
	}
	if options.ShowRemainingItemCount {
		meta.RemainingItemCount = &remainCount
	}

	cleaner := fields.NewCleaner(req.Fields)
	logger := logging.RequireLoggerFromContext(ctx)
	if s.offloadNodeStatusRepo.IsEnabled() && !cleaner.WillExclude("items.status.nodes") {
		offloadedNodes, err := s.offloadNodeStatusRepo.List(ctx, req.Namespace)
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
		for i, wf := range wfs {
			if wf.Status.IsOffloadNodeStatus() {
				if s.offloadNodeStatusRepo.IsEnabled() {
					wfs[i].Status.Nodes = offloadedNodes[sqldb.UUIDVersion{UID: string(wf.UID), Version: wf.GetOffloadNodeStatusVersion()}]
				} else {
					logger.WithFields(logging.Fields{"namespace": wf.Namespace, "name": wf.Name}).Warn(ctx, sqldb.OffloadNodeStatusDisabled)
				}
			}
		}
	}

	// we make no promises about the overall list sorting, we just sort each page
	sort.Sort(wfs)

	res := &wfv1.WorkflowList{ListMeta: meta, Items: wfs}
	newRes := &wfv1.WorkflowList{}
	if ok, err := cleaner.Clean(res, &newRes); err != nil {
		return nil, sutils.ToStatusError(fmt.Errorf("unable to CleanFields in request: %w", err), codes.Internal)
	} else if ok {
		return newRes, nil
	}
	return res, nil
}

func (s *workflowServer) WatchWorkflows(req *workflowpkg.WatchWorkflowsRequest, ws workflowpkg.WorkflowService_WatchWorkflowsServer) error {
	ctx := ws.Context()
	wfClient := auth.GetWfClient(ctx)
	opts := &metav1.ListOptions{}
	if req.ListOptions != nil {
		opts = req.ListOptions
		wfName := argoutil.RecoverWorkflowNameFromSelectorStringIfAny(opts.FieldSelector)
		if wfName != "" {
			// If we are using an alias (such as `@latest`) we need to dereference it.
			// s.getWorkflow does that for us
			wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, wfName, metav1.GetOptions{})
			if err != nil {
				return sutils.ToStatusError(err, codes.Internal)
			}
			opts.FieldSelector = argoutil.GenerateFieldSelectorFromWorkflowName(wf.Name)
		}
	}
	s.instanceIDService.With(opts)
	wfIf := wfClient.ArgoprojV1alpha1().Workflows(req.Namespace)
	watch, err := wfIf.Watch(ctx, *opts)
	if err != nil {
		return sutils.ToStatusError(err, codes.Internal)
	}
	defer watch.Stop()
	cleaner := fields.NewCleaner(req.Fields).WithoutPrefix("result.object.")

	clean := func(x *wfv1.Workflow) (*wfv1.Workflow, error) {
		y := &wfv1.Workflow{}
		if clean, err := cleaner.Clean(x, y); err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		} else if clean {
			return y, nil
		} else {
			return x, nil
		}
	}
	logger := logging.RequireLoggerFromContext(ctx)
	logger.Debug(ctx, "Piping events to channel")
	defer logger.Debug(ctx, "Result channel done")

	// Eagerly send the headers so that we can begin our keepalive loop if no results are received
	// immediately.  Without this, we cannot detect a streaming response, and we can't write to the
	// response since a subsequent write by the stream causes an error.
	err = ws.SendHeader(metadata.MD{})

	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, open := <-watch.ResultChan():
			if !open {
				return sutils.ToStatusError(io.EOF, codes.ResourceExhausted)
			}
			logger.Debug(ctx, "Received workflow event")
			wf, ok := event.Object.(*wfv1.Workflow)
			if !ok {
				// object is probably metav1.Status, `FromObject` can deal with anything
				return sutils.ToStatusError(apierr.FromObject(event.Object), codes.Internal)
			}
			if !cleaner.WillExclude("status.nodes") {
				if err := s.hydrator.Hydrate(ctx, wf); err != nil {
					return sutils.ToStatusError(err, codes.Internal)
				}
			}
			newWf, err := clean(wf)
			if err != nil {
				return sutils.ToStatusError(fmt.Errorf("unable to CleanFields in request: %w", err), codes.Internal)
			}
			logger.WithFields(logging.Fields{"workflow": wf.Name, "type": event.Type, "phase": wf.Status.Phase}).Debug(ctx, "Sending workflow event")
			err = ws.Send(&workflowpkg.WorkflowWatchEvent{Type: string(event.Type), Object: newWf})
			if err != nil {
				return sutils.ToStatusError(err, codes.Internal)
			}
		}
	}
}

func (s *workflowServer) WatchEvents(req *workflowpkg.WatchEventsRequest, ws workflowpkg.WorkflowService_WatchEventsServer) error {
	ctx := ws.Context()
	kubeClient := auth.GetKubeClient(ctx)
	opts := &metav1.ListOptions{}
	if req.ListOptions != nil {
		opts = req.ListOptions
	}
	s.instanceIDService.With(opts)
	eventInterface := kubeClient.CoreV1().Events(req.Namespace)
	watch, err := eventInterface.Watch(ctx, *opts)
	if err != nil {
		return sutils.ToStatusError(err, codes.Internal)
	}
	defer watch.Stop()

	logger := logging.RequireLoggerFromContext(ctx)
	logger.Debug(ctx, "Piping events to channel")
	defer logger.Debug(ctx, "Result channel done")

	err = ws.SendHeader(metadata.MD{})

	if err != nil {
		return sutils.ToStatusError(err, codes.Internal)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, open := <-watch.ResultChan():
			if !open {
				return sutils.ToStatusError(io.EOF, codes.ResourceExhausted)
			}
			logger.Debug(ctx, "Received event")
			e, ok := event.Object.(*corev1.Event)
			if !ok {
				// object is probably metav1.Status, `FromObject` can deal with anything
				return sutils.ToStatusError(apierr.FromObject(event.Object), codes.Internal)
			}
			logger.Debug(ctx, "Sending event")
			err = ws.Send(e)
			if err != nil {
				return sutils.ToStatusError(err, codes.Internal)
			}
		}
	}
}

func (s *workflowServer) DeleteWorkflow(ctx context.Context, req *workflowpkg.WorkflowDeleteRequest) (*workflowpkg.WorkflowDeleteResponse, error) {
	wfClient := auth.GetWfClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}
	if req.Force {
		_, err := auth.GetWfClient(ctx).ArgoprojV1alpha1().Workflows(wf.Namespace).Patch(ctx, wf.Name, types.MergePatchType, []byte("{\"metadata\":{\"finalizers\":null}}"), metav1.PatchOptions{})
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
	}
	err = auth.GetWfClient(ctx).ArgoprojV1alpha1().Workflows(wf.Namespace).Delete(ctx, wf.Name, metav1.DeleteOptions{PropagationPolicy: argoutil.GetDeletePropagation()})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	return &workflowpkg.WorkflowDeleteResponse{}, nil
}

func errorFromChannel(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
	}
	return nil
}

func (s *workflowServer) RetryWorkflow(ctx context.Context, req *workflowpkg.WorkflowRetryRequest) (*wfv1.Workflow, error) {
	logger := logging.RequireLoggerFromContext(ctx)
	wfClient := auth.GetWfClient(ctx)
	kubeClient := auth.GetKubeClient(ctx)

	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	err = s.hydrator.Hydrate(ctx, wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wf, podsToDelete, err := util.FormulateRetryWorkflow(ctx, wf, req.RestartSuccessful, req.NodeFieldSelector, req.Parameters)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	errCh := make(chan error, len(podsToDelete))
	var wg sync.WaitGroup
	wg.Add(len(podsToDelete))
	for _, podName := range podsToDelete {
		logger.WithFields(logging.Fields{"podDeleted": podName}).Info(ctx, "Deleting pod")
		go func(podName string) {
			defer wg.Done()
			err := kubeClient.CoreV1().Pods(wf.Namespace).Delete(ctx, podName, metav1.DeleteOptions{})
			if err != nil && !apierr.IsNotFound(err) {
				errCh <- err
				return
			}
		}(podName)
	}
	wg.Wait()

	err = errorFromChannel(errCh)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	err = s.hydrator.Dehydrate(ctx, wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Update(ctx, wf, metav1.UpdateOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	return wf, nil
}

func (s *workflowServer) ResubmitWorkflow(ctx context.Context, req *workflowpkg.WorkflowResubmitRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	newWF, err := util.FormulateResubmitWorkflow(ctx, wf, req.Memoized, req.Parameters)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	creator.LabelCreator(ctx, newWF)

	created, err := util.SubmitWorkflow(ctx, wfClient.ArgoprojV1alpha1().Workflows(req.Namespace), wfClient, req.Namespace, newWF, s.wfDefaults, &wfv1.SubmitOpts{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	return created, nil
}

func (s *workflowServer) ResumeWorkflow(ctx context.Context, req *workflowpkg.WorkflowResumeRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	err = util.ResumeWorkflow(ctx, wfClient.ArgoprojV1alpha1().Workflows(req.Namespace), s.hydrator, wf.Name, req.NodeFieldSelector)
	if err != nil {
		logger := logging.RequireLoggerFromContext(ctx)
		logger.WithFields(logging.Fields{"name": wf.Name}).WithError(err).Warn(ctx, "Failed to resume")
		return nil, sutils.ToStatusError(err, codes.Internal)

	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	return wf, nil
}

func (s *workflowServer) SuspendWorkflow(ctx context.Context, req *workflowpkg.WorkflowSuspendRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)

	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	err = util.SuspendWorkflow(ctx, wfClient.ArgoprojV1alpha1().Workflows(wf.Namespace), wf.Name)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	return wf, nil
}

func (s *workflowServer) TerminateWorkflow(ctx context.Context, req *workflowpkg.WorkflowTerminateRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)

	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	err = util.TerminateWorkflow(ctx, wfClient.ArgoprojV1alpha1().Workflows(req.Namespace), wf.Name)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	return wf, nil
}

func (s *workflowServer) StopWorkflow(ctx context.Context, req *workflowpkg.WorkflowStopRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}
	err = util.StopWorkflow(ctx, wfClient.ArgoprojV1alpha1().Workflows(req.Namespace), s.hydrator, wf.Name, req.NodeFieldSelector, req.Message)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	return wf, nil
}

func (s *workflowServer) SetWorkflow(ctx context.Context, req *workflowpkg.WorkflowSetRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	err = s.validateWorkflow(wf)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	phaseToSet := wfv1.NodePhase(req.Phase)
	switch phaseToSet {
	case wfv1.NodeSucceeded, wfv1.NodeFailed, wfv1.NodeError, "":
		// Do nothing, passes validation
	default:
		return nil, sutils.ToStatusError(fmt.Errorf("%s is an invalid phase to set to", req.Phase), codes.InvalidArgument)
	}

	outputParams := make(map[string]string)
	if req.OutputParameters != "" {
		err = json.Unmarshal([]byte(req.OutputParameters), &outputParams)
		if err != nil {
			return nil, sutils.ToStatusError(fmt.Errorf("unable to parse output parameter set request: %s", err), codes.InvalidArgument)
		}
	}

	operation := util.SetOperationValues{
		Phase:            phaseToSet,
		Message:          req.Message,
		OutputParameters: outputParams,
	}

	err = util.SetWorkflow(ctx, wfClient.ArgoprojV1alpha1().Workflows(req.Namespace), s.hydrator, wf.Name, req.NodeFieldSelector, operation)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	return wf, nil
}

func (s *workflowServer) LintWorkflow(ctx context.Context, req *workflowpkg.WorkflowLintRequest) (*wfv1.Workflow, error) {
	if req.Workflow == nil {
		return nil, fmt.Errorf("unable to get a workflow")
	}
	wftmplGetter := s.wftmplStore.Getter(ctx, req.Workflow.Namespace)
	cwftmplGetter := s.cwftmplStore.Getter(ctx)
	s.instanceIDService.Label(req.Workflow)
	creator.LabelCreator(ctx, req.Workflow)

	err := validate.ValidateWorkflow(ctx, wftmplGetter, cwftmplGetter, req.Workflow, s.wfDefaults, validate.ValidateOpts{Lint: true})
	if err != nil {
		return nil, err
	}

	return req.Workflow, nil
}

func (s *workflowServer) PodLogs(req *workflowpkg.WorkflowLogRequest, ws workflowpkg.WorkflowService_PodLogsServer) error {
	ctx := ws.Context()
	wfClient := auth.GetWfClient(ctx)
	kubeClient := auth.GetKubeClient(ctx)
	wf, err := s.getWorkflow(ctx, wfClient, req.Namespace, req.Name, metav1.GetOptions{})
	if err != nil {
		return sutils.ToStatusError(err, codes.Internal)
	}
	err = s.validateWorkflow(wf)
	if err != nil {
		return sutils.ToStatusError(err, codes.InvalidArgument)
	}
	req.Name = wf.Name

	err = ws.SendHeader(metadata.MD{})
	if err != nil {
		return sutils.ToStatusError(err, codes.Internal)
	}

	err = logs.WorkflowLogs(ctx, wfClient, kubeClient, req, ws)
	return sutils.ToStatusError(err, codes.Internal)
}

func (s *workflowServer) WorkflowLogs(req *workflowpkg.WorkflowLogRequest, ws workflowpkg.WorkflowService_WorkflowLogsServer) error {
	return sutils.ToStatusError(s.PodLogs(req, ws), codes.Internal)
}

func (s *workflowServer) getWorkflow(ctx context.Context, wfClient versioned.Interface, namespace string, name string, options metav1.GetOptions) (*wfv1.Workflow, error) {
	logger := logging.RequireLoggerFromContext(ctx)
	if name == latestAlias {
		latest, err := getLatestWorkflow(ctx, wfClient, namespace)
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
		logger.WithFields(logging.Fields{"alias": latestAlias, "workflow": latest.Name}).Debug(ctx, "Resolved alias to workflow")
		return latest, nil
	}

	wf, origErr := wfClient.ArgoprojV1alpha1().Workflows(namespace).Get(ctx, name, options)
	// fallback to retrieve from archived workflows
	if wf == nil || origErr != nil {
		allowed, err := auth.CanI(ctx, "get", workflow.WorkflowPlural, namespace, name)
		if err != nil {
			return nil, getWorkflowOrigErr(ctx, origErr, err)
		}
		if !allowed {
			err = status.Error(codes.PermissionDenied, "permission denied")
			return nil, getWorkflowOrigErr(ctx, origErr, err)
		}

		wf, err = s.wfArchive.GetWorkflow(ctx, "", namespace, name)
		if wf == nil || err != nil {
			return nil, getWorkflowOrigErr(ctx, origErr, err)
		}
	}
	return wf, nil
}

// getWorkflowOrigErr only returns the original error to preserve the original status code
// it logs out the new error
func getWorkflowOrigErr(ctx context.Context, origErr error, err error) error {
	logger := logging.RequireLoggerFromContext(ctx)
	logger.WithFields(logging.Fields{"liveError": origErr, "archivedError": err}).Error(ctx, "failed to get live workflow and failed to get archived workflow")
	return sutils.ToStatusError(origErr, codes.Internal)
}

func (s *workflowServer) validateWorkflow(wf *wfv1.Workflow) error {
	return sutils.ToStatusError(s.instanceIDService.Validate(wf), codes.InvalidArgument)
}

func getLatestWorkflow(ctx context.Context, wfClient versioned.Interface, namespace string) (*wfv1.Workflow, error) {
	wfList, err := wfClient.ArgoprojV1alpha1().Workflows(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}
	if len(wfList.Items) < 1 {
		return nil, sutils.ToStatusError(fmt.Errorf("no workflows found"), codes.NotFound)
	}
	latest := wfList.Items[0]
	for _, wf := range wfList.Items {
		if latest.CreationTimestamp.Before(&wf.CreationTimestamp) {
			latest = wf
		}
	}
	return &latest, nil
}

func (s *workflowServer) SubmitWorkflow(ctx context.Context, req *workflowpkg.WorkflowSubmitRequest) (*wfv1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	var wf *wfv1.Workflow
	switch req.ResourceKind {
	case workflow.CronWorkflowKind, workflow.CronWorkflowSingular, workflow.CronWorkflowPlural, workflow.CronWorkflowShortName:
		cronWf, err := wfClient.ArgoprojV1alpha1().CronWorkflows(req.Namespace).Get(ctx, req.ResourceName, metav1.GetOptions{})
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.Internal)
		}
		wf = common.ConvertCronWorkflowToWorkflow(cronWf)
	case workflow.WorkflowTemplateKind, workflow.WorkflowTemplateSingular, workflow.WorkflowTemplatePlural, workflow.WorkflowTemplateShortName:
		wf = common.NewWorkflowFromWorkflowTemplate(req.ResourceName, false)
	case workflow.ClusterWorkflowTemplateKind, workflow.ClusterWorkflowTemplateSingular, workflow.ClusterWorkflowTemplatePlural, workflow.ClusterWorkflowTemplateShortName:
		wf = common.NewWorkflowFromWorkflowTemplate(req.ResourceName, true)
	default:
		err := errors.Errorf(errors.CodeBadRequest, "Resource kind '%s' is not supported for submitting", req.ResourceKind)
		err = sutils.ToStatusError(err, codes.InvalidArgument)
		return nil, err
	}

	s.instanceIDService.Label(wf)
	creator.LabelCreator(ctx, wf)
	err := util.ApplySubmitOpts(wf, req.SubmitOptions)
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.Internal)
	}

	wftmplGetter := s.wftmplStore.Getter(ctx, req.Namespace)
	cwftmplGetter := s.cwftmplStore.Getter(ctx)

	err = validate.ValidateWorkflow(ctx, wftmplGetter, cwftmplGetter, wf, s.wfDefaults, validate.ValidateOpts{Submit: true})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}

	// if we are doing a normal dryRun, just return the workflow un-altered
	if req.SubmitOptions != nil && req.SubmitOptions.DryRun {
		return wf, nil
	}
	if req.SubmitOptions != nil && req.SubmitOptions.ServerDryRun {
		// For a server dry run we require a namespace
		if wf.Namespace == "" {
			wf.Namespace = req.Namespace
		}
		workflow, err := util.CreateServerDryRun(ctx, wf, wfClient)
		if err != nil {
			return nil, sutils.ToStatusError(err, codes.InvalidArgument)
		}
		return workflow, nil
	}

	wf, err = wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Create(ctx, wf, metav1.CreateOptions{})
	if err != nil {
		return nil, sutils.ToStatusError(err, codes.InvalidArgument)
	}
	return wf, nil
}
