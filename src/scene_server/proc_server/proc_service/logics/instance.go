/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package logics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/mapstr"
	"configcenter/src/common/metadata"
)

//var handEventDataChan chan chanItem // := make(chan chanItem, 10000)

func (lgc *Logics) HandleHostProcDataChange(ctx context.Context, eventData *metadata.EventInst) {

	switch eventData.ObjType {
	case metadata.EventObjTypeProcModule:

		handEventDataChan <- chanItem{ctx: ctx, eventData: eventData, opFunc: lgc.eventProcInstByProcModule, retry: 3}
		//lgc.handleRetry(3, ctx, eventData, lgc.refreshProcInstByProcModule)
		//lgc.refreshProcInstByProcModule(ctx, eventData)
	case metadata.EventObjTypeModuleTransfer:
		handEventDataChan <- chanItem{ctx: ctx, eventData: eventData, opFunc: lgc.eventHostModuleChangeProcHostInstNum, retry: 3}
		//lgc.handleRetry(3, ctx, eventData, lgc.eventHostModuleChangeProcHostInstNum)
		//lgc.eventHostModuleChangeProcHostInstNum(ctx, eventData)
	case common.BKInnerObjIDHost:
		handEventDataChan <- chanItem{ctx: ctx, eventData: eventData, opFunc: lgc.eventProcInstByHostInfo, retry: 3}
	case common.BKInnerObjIDProc:
		handEventDataChan <- chanItem{ctx: ctx, eventData: eventData, opFunc: lgc.eventProcInstByProcess, retry: 3}

	}
	chnOpLock.Do(lgc.bgHandle)

}

func (lgc *Logics) bgHandle() {

	go func() {
		defer lgc.bgHandle()
		for {
			select {
			case item := <-handEventDataChan:
				for idx := 0; idx < item.retry; idx++ {
					err := item.opFunc(item.ctx, item.eventData)
					if nil == err {
						break
					}
				}
			case <-eventRefreshModuleData.eventChn:
				for {
					item := getEventRefreshModuleItem()
					if nil == item {
						break
					}
					err := lgc.HandleProcInstNumByModuleID(context.Background(), item.header, item.appID, item.moduleID)
					if nil != err {
						blog.Errorf("HandleProcInstNumByModuleID  error %s", err.Error())
					}
				}
			}

		}

	}()

}

func (lgc *Logics) HandleProcInstNumByModuleID(ctx context.Context, header http.Header, appID, moduleID int64) error {
	maxInstID, procInst, err := lgc.getProcInstInfoByModuleID(ctx, appID, moduleID, header)
	if nil != err {
		return err
	}
	var hostInfos map[int64]*metadata.GseHost
	hostInfos, err = lgc.getHostByModuleID(ctx, header, moduleID)
	if nil != err {
		blog.Errorf("handleInstanceNum getHostByModuleID error %s", err.Error())
		return err
	}
	setID, procIDs, err := lgc.getModuleBindProc(ctx, appID, moduleID, header)
	if nil != err {
		return err
	}
	instProc := make([]*metadata.ProcInstanceModel, 0)
	procInfos, err := lgc.getProcInfoByID(ctx, procIDs, header)
	isExistHostInst := make(map[string]metadata.ProcInstanceModel)
	for procID, info := range procInfos {
		for hostID, _ := range hostInfos {
			procInstInfo, ok := procInst[getInlineProcInstKey(hostID, procID)]
			hostInstID := uint64(0)
			if !ok {
				maxInstID++
				hostInstID = maxInstID
			} else {
				hostInstID = procInstInfo.HostInstanID
				isExistHostInst[getInlineProcInstKey(hostID, procID)] = procInstInfo
			}
			instProc = append(instProc, GetProcInstModel(appID, setID, moduleID, hostID, procID, info.FunID, info.FunID, hostInstID)...)
		}

	}
	unregisterProcDetail := make([]metadata.ProcInstanceModel, 0)
	for key, info := range procInst {
		_, ok := isExistHostInst[key]
		if !ok {
			unregisterProcDetail = append(unregisterProcDetail, info)
		}
	}

	err = lgc.setProcInstDetallStatusUnregister(ctx, header, appID, moduleID, unregisterProcDetail)
	if nil != err {
		return err
	}
	err = lgc.handleProcInstNumDataHandle(ctx, header, appID, moduleID, procIDs, instProc)
	if nil != err {
		return err
	}
	for _, info := range procInfos {
		gseHost := make([]metadata.GseHost, 0)
		for _, host := range hostInfos {
			gseHost = append(gseHost, *host)
		}
		if 0 == len(gseHost) {
			err := lgc.RegisterProcInstanceToGse(moduleID, gseHost, info.ProcInfo, header)
			if nil != err {
				blog.Errorf("RegisterProcInstanceToGse error%s", err.Error())
				return err
			}
		}
	}

	err = lgc.unregisterProcInstDetall(ctx, header, appID, moduleID, unregisterProcDetail)
	if nil != err {
		return err
	}

	return nil
}

func (lgc *Logics) eventHostModuleChangeProcHostInstNum(ctx context.Context, eventData *metadata.EventInst) error {
	var header http.Header = make(http.Header, 0)
	header.Set(common.BKHTTPOwnerID, eventData.OwnerID)
	header.Set(common.BKHTTPHeaderUser, common.BKProcInstanceOpUser)

	for _, hostInfos := range eventData.Data {
		var data interface{}
		if metadata.EventActionDelete == eventData.Action {
			data = hostInfos.PreData
		} else {
			data = hostInfos.CurData
		}
		mapData, err := mapstr.NewFromInterface(data)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("productHostInstanceNum event data not map[string]interface{} item %v raw josn %s", hostInfos, string(byteData))
			return err
		}
		appID, err := mapData.Int64(common.BKAppIDField)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("productHostInstanceNum event data  appID not integer  item %v raw josn %s", hostInfos, string(byteData))
			return err
		}
		moduleID, err := mapData.Int64(common.BKModuleIDField)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("productHostInstanceNum event data  moduleID not integer  item %v raw josn %s", hostInfos, string(byteData))
			return err
		}
		addEventRefreshModuleItem(appID, moduleID, header)

	}
	sendEventFrefreshModuleNotice()
	return nil
}

func (lgc *Logics) eventProcInstByProcModule(ctx context.Context, eventData *metadata.EventInst) error {
	if metadata.EventTypeRelation != eventData.EventType {
		return nil
	}
	var header http.Header = make(http.Header, 0)
	header.Set(common.BKHTTPOwnerID, eventData.OwnerID)
	header.Set(common.BKHTTPHeaderUser, common.BKProcInstanceOpUser)

	for _, data := range eventData.Data {
		var iData interface{}
		if metadata.EventActionDelete == eventData.Action {
			// delete process bind module relation, unregister process info
			iData = data.PreData
		} else {
			// compare  pre-change data with the current data and find the newly added data to register process info
			iData = data.CurData
		}
		mapData, err := mapstr.NewFromInterface(iData)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcModule event data not map[string]interface{} item %v raw josn %s", data, string(byteData))
			return err
		}
		appID, err := mapData.Int64(common.BKAppIDField)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcModule event data  appID not integer  item %v raw josn %s", data, string(byteData))
			return err
		}
		moduleName, err := mapData.String(common.BKModuleNameField)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcModule event data  appID not integer  item %v raw josn %s", data, string(byteData))
			return err
		}

		moduleID, err := lgc.HandleProcInstNumByModuleName(ctx, header, appID, moduleName)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcModule HandleProcInstNumByModuleName error %s item %v raw josn %s", err.Error(), data, string(byteData))
			return err
		}
		addEventRefreshModuleItems(appID, moduleID, header)
	}
	return nil
}

func (lgc *Logics) eventProcInstByProcess(ctx context.Context, eventData *metadata.EventInst) error {
	if metadata.EventActionCreate != eventData.Action {
		// create proccess not refresh process instance , because not bind module
		return nil
	}
	var header http.Header = make(http.Header, 0)
	header.Set(common.BKHTTPOwnerID, eventData.OwnerID)
	header.Set(common.BKHTTPHeaderUser, common.BKProcInstanceOpUser)

	for _, data := range eventData.Data {
		mapData, err := mapstr.NewFromInterface(data.CurData)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcess event data not map[string]interface{} item %v raw josn %s", data, string(byteData))
			return err
		}
		procID, err := mapData.Int64(common.BKProcIDField)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcess process id not integer error %s item %v raw josn %s", err.Error(), data, string(byteData))
			return err
		}
		appID, err := mapData.Int64(common.BKAppIDField)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcess application id not integer error %s item %v raw josn %s", err.Error(), data, string(byteData))
			return err
		}
		mdouleID, err := lgc.getModuleIDByProcID(ctx, appID, procID, header)
		if nil != err {
			byteData, _ := json.Marshal(eventData)
			blog.Errorf("eventProcInstByProcess get process bind module info  by appID %d, procID %d error %s item %v raw josn %s", appID, procID, err.Error(), data, string(byteData))
			return err
		}
		addEventRefreshModuleItems(appID, mdouleID, header)
	}
	sendEventFrefreshModuleNotice()
	return nil
}

func (lgc *Logics) eventProcInstByHostInfo(ctx context.Context, eventData *metadata.EventInst) error {

	if metadata.EventActionUpdate == eventData.Action {
		var header http.Header = make(http.Header, 0)
		header.Set(common.BKHTTPOwnerID, eventData.OwnerID)
		header.Set(common.BKHTTPHeaderUser, common.BKProcInstanceOpUser)

		// host clouid id change
		for _, data := range eventData.Data {
			mapCurData, err := mapstr.NewFromInterface(data.CurData)
			if nil != err {
				byteData, _ := json.Marshal(eventData)
				blog.Errorf("eventProcInstByHostInfo event current data not map[string]interface{} item %v raw josn %s", data, string(byteData))
				return err
			}

			mapPreData, err := mapstr.NewFromInterface(data.PreData)
			if nil != err {
				byteData, _ := json.Marshal(eventData)
				blog.Errorf("eventProcInstByHostInfo event pre-data not map[string]interface{} item %v raw josn %s", data, string(byteData))
				return err
			}
			curData, err := mapCurData.Int64(common.BKCloudIDField)
			if nil != err {
				byteData, _ := json.Marshal(eventData)
				blog.Errorf("eventProcInstByHostInfo event current data cloud id not int item %v raw josn %s", data, string(byteData))
				return err
			}
			preData, err := mapPreData.Int64(common.BKCloudIDField)
			if nil != err {
				byteData, _ := json.Marshal(eventData)
				blog.Errorf("eventProcInstByHostInfo event pre-data  cloud id not int item %v raw josn %s", data, string(byteData))
				return err
			}
			if curData != preData {
				hostID, err := mapCurData.Int64(common.BKHostIDField)
				if nil != err {
					byteData, _ := json.Marshal(eventData)
					blog.Errorf("eventProcInstByHostInfo event hostID not int item %v raw josn %s", data, string(byteData))
					return err
				}
				hostModule, err := lgc.GetModuleIDByHostID(ctx, header, hostID)
				if nil != err {
					byteData, _ := json.Marshal(eventData)
					blog.Errorf("eventProcInstByHostInfo event hostID %s get module err :%s  item %v raw josn %s", hostID, err.Error(), data, string(byteData))
					return err
				}
				for _, item := range hostModule {
					addEventRefreshModuleItem(item.AppID, item.ModuleID, header)
				}
			}
		}
	}
	sendEventFrefreshModuleNotice()
	return nil
}

func (lgc *Logics) GetModuleIDByHostID(ctx context.Context, header http.Header, hostID int64) ([]metadata.ModuleHost, error) {
	dat := map[string][]int64{
		common.BKHostIDField: []int64{hostID},
	}
	ret, err := lgc.CoreAPI.HostController().Module().GetModulesHostConfig(ctx, header, dat)
	if nil != err {
		blog.Errorf("GetModuleIDByHostID appID %d module id %d GetModulesHostConfig http do error:%s", hostID, err.Error())
		return nil, err
	}
	if !ret.Result {
		blog.Errorf("GetModuleIDByHostID appID %d module id %d GetModulesHostConfig reply error:%s", hostID, ret.ErrMsg)
		return nil, fmt.Errorf(ret.ErrMsg)
	}

	return ret.Data, nil
}
