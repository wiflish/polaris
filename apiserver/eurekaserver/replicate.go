/**
 * Tencent is pleased to support the open source community by making Polaris available.
 *
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software distributed
 * under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
 * CONDITIONS OF ANY KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations under the License.
 */

package eurekaserver

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/emicklei/go-restful/v3"
	"github.com/golang/protobuf/ptypes/wrappers"
	apiservice "github.com/polarismesh/specification/source/go/api/v1/service_manage"

	api "github.com/polarismesh/polaris/common/api/v1"
	"github.com/polarismesh/polaris/common/model"
	"github.com/polarismesh/polaris/common/utils"
)

const (
	actionRegister             = "Register"
	actionHeartbeat            = "Heartbeat"
	actionCancel               = "Cancel"
	actionStatusUpdate         = "StatusUpdate"
	actionDeleteStatusOverride = "DeleteStatusOverride"
)

const (
	headerIdentityName    = "DiscoveryIdentity-Name"
	headerIdentityVersion = "DiscoveryIdentity-Version"
	headerIdentityId      = "DiscoveryIdentity-Id"
	valueIdentityName     = "PolarisServer"
)

// BatchReplication do the server request replication
func (h *EurekaServer) BatchReplication(req *restful.Request, rsp *restful.Response) {
	log.Infof("[EUREKA-SERVER] received replicate request %+v", req)
	sourceSvrName := req.HeaderParameter(headerIdentityName)
	remoteAddr := req.Request.RemoteAddr
	if sourceSvrName == valueIdentityName {
		// we should not process the replication from polaris
		batchResponse := &ReplicationListResponse{ResponseList: []*ReplicationInstanceResponse{}}
		if err := writeEurekaResponse(restful.MIME_JSON, batchResponse, req, rsp); nil != err {
			log.Errorf("[EurekaServer]fail to write replicate response, client: %s, err: %v", remoteAddr, err)
		}
		return
	}
	replicateRequest := &ReplicationList{}
	var err error
	err = req.ReadEntity(replicateRequest)
	if nil != err {
		log.Errorf("[EUREKA-SERVER] fail to parse peer replicate request, uri: %s, client: %s, err: %v",
			req.Request.RequestURI, remoteAddr, err)
		writePolarisStatusCode(req, api.ParseException)
		writeHeader(http.StatusBadRequest, rsp)
		return
	}
	token, err := getAuthFromEurekaRequestHeader(req)
	if err != nil {
		log.Infof("[EUREKA-SERVER]replicate request get basic auth info fail, code is %d", api.ExecuteException)
		writePolarisStatusCode(req, api.ExecuteException)
		writeHeader(http.StatusForbidden, rsp)
		return
	}
	batchResponse, resultCode := h.doBatchReplicate(replicateRequest, token)
	if err := writeEurekaResponseWithCode(restful.MIME_JSON, batchResponse, req, rsp, resultCode); nil != err {
		log.Errorf("[EurekaServer]fail to write replicate response, client: %s, err: %v", remoteAddr, err)
	}
}

func (h *EurekaServer) doBatchReplicate(
	replicateRequest *ReplicationList, token string) (*ReplicationListResponse, uint32) {
	batchResponse := &ReplicationListResponse{ResponseList: []*ReplicationInstanceResponse{}}
	var resultCode = api.ExecuteSuccess
	for _, instanceInfo := range replicateRequest.ReplicationList {
		resp, code := h.dispatch(instanceInfo, token)
		if code != api.ExecuteSuccess {
			resultCode = code
			log.Warnf("[EUREKA-SERVER] fail to process replicate instance request, code is %d, action %s, instance %s, app %s",
				code, instanceInfo.Action, instanceInfo.Id, instanceInfo.AppName)
		}
		batchResponse.ResponseList = append(batchResponse.ResponseList, resp)
	}
	return batchResponse, resultCode
}

func (h *EurekaServer) dispatch(
	replicationInstance *ReplicationInstance, token string) (*ReplicationInstanceResponse, uint32) {
	appName := formatReadName(replicationInstance.AppName)
	ctx := context.WithValue(context.Background(), utils.ContextAuthTokenKey, token)
	var retCode = api.ExecuteSuccess
	log.Debugf("[EurekaServer]dispatch replicate request %+v", replicationInstance)
	if nil != replicationInstance.InstanceInfo {
		_ = convertInstancePorts(replicationInstance.InstanceInfo)
		log.Debugf("[EurekaServer]dispatch replicate instance %+v, port %+v, sport %+v",
			replicationInstance.InstanceInfo, replicationInstance.InstanceInfo.Port, replicationInstance.InstanceInfo.SecurePort)
	}
	switch replicationInstance.Action {
	case actionRegister:
		instanceInfo := replicationInstance.InstanceInfo
		retCode = h.registerInstances(ctx, appName, instanceInfo, true)
		if retCode == api.ExecuteSuccess || retCode == api.ExistedResource || retCode == api.SameInstanceRequest {
			retCode = api.ExecuteSuccess
		}
	case actionHeartbeat:
		instanceId := replicationInstance.Id
		retCode = h.renew(ctx, appName, instanceId, true)
		if retCode == api.ExecuteSuccess || retCode == api.HeartbeatExceedLimit {
			retCode = api.ExecuteSuccess
		}
	case actionCancel:
		instanceId := replicationInstance.Id
		retCode = h.deregisterInstance(ctx, appName, instanceId, true)
		if retCode == api.ExecuteSuccess || retCode == api.NotFoundResource || retCode == api.SameInstanceRequest {
			retCode = api.ExecuteSuccess
		}
	case actionStatusUpdate:
		status := replicationInstance.Status
		instanceId := replicationInstance.Id
		retCode = h.updateStatus(ctx, appName, instanceId, status, true)
	case actionDeleteStatusOverride:
		instanceId := replicationInstance.Id
		retCode = h.updateStatus(ctx, appName, instanceId, StatusUp, true)
	}

	statusCode := http.StatusOK
	if retCode == api.NotFoundResource {
		statusCode = http.StatusNotFound
	}
	return &ReplicationInstanceResponse{
		StatusCode: statusCode,
	}, retCode
}

func eventToInstance(event *model.InstanceEvent, appName string, curTimeMilli int64) *InstanceInfo {
	instance := &apiservice.Instance{
		Id:                &wrappers.StringValue{Value: event.Id},
		Host:              &wrappers.StringValue{Value: event.Instance.GetHost().GetValue()},
		Port:              &wrappers.UInt32Value{Value: event.Instance.GetPort().GetValue()},
		Protocol:          &wrappers.StringValue{Value: event.Instance.GetProtocol().GetValue()},
		Version:           &wrappers.StringValue{Value: event.Instance.GetVersion().GetValue()},
		Priority:          &wrappers.UInt32Value{Value: event.Instance.GetPriority().GetValue()},
		Weight:            &wrappers.UInt32Value{Value: event.Instance.GetWeight().GetValue()},
		EnableHealthCheck: &wrappers.BoolValue{Value: event.Instance.GetEnableHealthCheck().GetValue()},
		HealthCheck:       event.Instance.GetHealthCheck(),
		Healthy:           &wrappers.BoolValue{Value: event.Instance.GetHealthy().GetValue()},
		Isolate:           &wrappers.BoolValue{Value: event.Instance.GetIsolate().GetValue()},
		Location:          event.Instance.GetLocation(),
		Metadata:          event.Instance.GetMetadata(),
	}
	if event.EType == model.EventInstanceTurnHealth {
		instance.Healthy = &wrappers.BoolValue{Value: true}
	} else if event.EType == model.EventInstanceTurnUnHealth {
		instance.Healthy = &wrappers.BoolValue{Value: false}
	} else if event.EType == model.EventInstanceOpenIsolate {
		instance.Isolate = &wrappers.BoolValue{Value: true}
	} else if event.EType == model.EventInstanceCloseIsolate {
		instance.Isolate = &wrappers.BoolValue{Value: false}
	}
	return buildInstance(appName, instance, curTimeMilli)
}

func (h *EurekaServer) shouldReplicate(e model.InstanceEvent) bool {
	if e.Namespace != h.namespace {
		// only process the service in same namespace
		return false
	}
	metadata := e.MetaData
	if len(metadata) > 0 {
		if value, ok := metadata[MetadataReplicate]; ok {
			// we should not replicate around
			isReplicate, _ := strconv.ParseBool(value)
			return !isReplicate
		}
	}
	return true
}

func (h *EurekaServer) handleInstanceEvent(ctx context.Context, i interface{}) error {
	e := i.(model.InstanceEvent)
	if !h.shouldReplicate(e) {
		return nil
	}
	appName := formatReadName(e.Service)
	curTimeMilli := time.Now().UnixMilli()
	switch e.EType {
	case model.EventInstanceOnline:
		instanceInfo := eventToInstance(&e, appName, curTimeMilli)
		h.replicateWorker.AddReplicateTask(&ReplicationInstance{
			AppName:            appName,
			Id:                 e.Id,
			LastDirtyTimestamp: curTimeMilli,
			Status:             StatusUp,
			InstanceInfo:       instanceInfo,
			Action:             actionRegister,
		})
	case model.EventInstanceOffline:
		h.replicateWorker.AddReplicateTask(&ReplicationInstance{
			AppName: appName,
			Id:      e.Id,
			Action:  actionCancel,
		})
	case model.EventInstanceSendHeartbeat:
		instanceInfo := eventToInstance(&e, appName, curTimeMilli)
		rInstance := &ReplicationInstance{
			AppName:      appName,
			Id:           e.Id,
			Status:       StatusUp,
			InstanceInfo: instanceInfo,
			Action:       actionHeartbeat,
		}
		if e.Instance.GetIsolate().GetValue() {
			rInstance.OverriddenStatus = StatusOutOfService
		}
		h.replicateWorker.AddReplicateTask(rInstance)
	case model.EventInstanceTurnHealth:
		h.replicateWorker.AddReplicateTask(&ReplicationInstance{
			AppName:            appName,
			Id:                 e.Id,
			LastDirtyTimestamp: curTimeMilli,
			Status:             StatusUp,
			Action:             actionStatusUpdate,
		})
	case model.EventInstanceTurnUnHealth:
		h.replicateWorker.AddReplicateTask(&ReplicationInstance{
			AppName:            appName,
			Id:                 e.Id,
			LastDirtyTimestamp: curTimeMilli,
			Status:             StatusDown,
			Action:             actionStatusUpdate,
		})
	case model.EventInstanceOpenIsolate:
		h.replicateWorker.AddReplicateTask(&ReplicationInstance{
			AppName:            appName,
			Id:                 e.Id,
			LastDirtyTimestamp: curTimeMilli,
			OverriddenStatus:   StatusOutOfService,
			Action:             actionHeartbeat,
		})
	case model.EventInstanceCloseIsolate:
		h.replicateWorker.AddReplicateTask(&ReplicationInstance{
			AppName:            appName,
			Id:                 e.Id,
			LastDirtyTimestamp: curTimeMilli,
			Action:             actionDeleteStatusOverride,
		})

	}
	return nil
}
