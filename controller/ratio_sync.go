package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

const (
	defaultTimeoutSeconds       = 10
	defaultEndpoint             = "/api/ratio_config"
	maxConcurrentFetches        = 8
	maxRatioConfigBytes         = 10 << 20 // 10MB
	floatEpsilon                = 1e-9
	officialRatioPresetID       = -100
	officialRatioPresetName     = "官方倍率预设"
	officialRatioPresetBaseURL  = "https://basellm.github.io"
	modelsDevPresetID           = -101
	modelsDevPresetName         = "models.dev 价格预设"
	modelsDevPresetBaseURL      = "https://models.dev"
	modelsDevHost               = "models.dev"
	modelsDevPath               = "/api.json"
	modelsDevInputCostRatioBase = 1000.0
)

func nearlyEqual(a, b float64) bool {
	if a > b {
		return a-b < floatEpsilon
	}
	return b-a < floatEpsilon
}

func valuesEqual(a, b interface{}) bool {
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok && bok {
		return nearlyEqual(af, bf)
	}
	return a == b
}

var ratioTypes = []string{"model_ratio", "completion_ratio", "cache_ratio", "model_price"}

type upstreamResult struct {
	Name        string            `json:"name"`
	Data        map[string]any    `json:"data,omitempty"`
	BillingExpr map[string]string `json:"billing_expr,omitempty"`
	BillingMode map[string]string `json:"billing_mode,omitempty"`
	Err         string            `json:"err,omitempty"`
}

// ratioPayload 是一次上游响应的解析结果：数值倍率与阶梯计费表达式分开存放。
// 表达式绝不会被当成倍率，否则前端会把它写进 ModelRatio 并触发有限数字校验错误。
type ratioPayload struct {
	Ratios      map[string]any    // ratioTypes 下的有限数字
	BillingExpr map[string]string // 阶梯计费表达式（billing_expr）
	BillingMode map[string]string // 对应的 billing_mode
}

func (p ratioPayload) hasBilling() bool { return len(p.BillingExpr) > 0 }

// parseRatioPayload 解析上游返回的 data 字段，支持三种格式：
//   - type1:   数值倍率对象，含 model_ratio / completion_ratio / cache_ratio / model_price 中至少一个
//   - billing: {"billing_expr": {...}, "billing_mode": {...}}，官方倍率预设就是这种格式
//   - type2:   /api/pricing 列表，逐项按 billing_mode 分流到数值倍率或表达式
//
// 返回 (payload, dropped, errMsg)：errMsg 非空表示格式无法识别；
// Ratios 里 ratioTypes 下的值保证都是有限数字。
func parseRatioPayload(data json.RawMessage) (ratioPayload, int, string) {
	// type1：数值倍率（优先，兼容同时带 billing_expr 的混合响应）
	var type1Data map[string]any
	if err := common.Unmarshal(data, &type1Data); err == nil {
		for _, ratioType := range ratioTypes {
			if _, ok := type1Data[ratioType]; ok {
				cleaned, dropped := sanitizeRatioData(type1Data)
				return ratioPayload{Ratios: cleaned}, dropped, ""
			}
		}
	}

	// billing：只提供表达式计费的上游（官方倍率预设 ratio_config-v1-base.json）
	var billingData struct {
		BillingExpr map[string]string `json:"billing_expr"`
		BillingMode map[string]string `json:"billing_mode"`
	}
	if err := common.Unmarshal(data, &billingData); err == nil && len(billingData.BillingExpr) > 0 {
		expr, mode := normalizeBillingMaps(billingData.BillingExpr, billingData.BillingMode)
		return ratioPayload{BillingExpr: expr, BillingMode: mode}, 0, ""
	}

	// type2：/api/pricing 列表
	var pricingItems []struct {
		ModelName       string   `json:"model_name"`
		QuotaType       int      `json:"quota_type"`
		ModelRatio      *float64 `json:"model_ratio"`
		ModelPrice      *float64 `json:"model_price"`
		CompletionRatio *float64 `json:"completion_ratio"`
		BillingMode     string   `json:"billing_mode"`
		BillingExpr     string   `json:"billing_expr"`
	}
	if err := common.Unmarshal(data, &pricingItems); err != nil {
		return ratioPayload{}, 0, "无法解析上游返回数据"
	}

	modelRatioMap := make(map[string]float64)
	completionRatioMap := make(map[string]float64)
	modelPriceMap := make(map[string]float64)
	billingExprMap := make(map[string]string)
	billingModeMap := make(map[string]string)

	for _, item := range pricingItems {
		name := strings.TrimSpace(item.ModelName)
		if name == "" {
			continue
		}
		if item.BillingMode == billing_setting.BillingModeTieredExpr && strings.TrimSpace(item.BillingExpr) != "" {
			billingExprMap[name] = item.BillingExpr
			billingModeMap[name] = billing_setting.BillingModeTieredExpr
			continue
		}
		if item.QuotaType == 1 {
			if item.ModelPrice != nil {
				modelPriceMap[name] = *item.ModelPrice
			}
			continue
		}
		if item.ModelRatio != nil {
			modelRatioMap[name] = *item.ModelRatio
		}
		if item.CompletionRatio != nil {
			completionRatioMap[name] = *item.CompletionRatio
		}
	}

	converted := make(map[string]any)
	if len(modelRatioMap) > 0 {
		ratioAny := make(map[string]any, len(modelRatioMap))
		for k, v := range modelRatioMap {
			ratioAny[k] = v
		}
		converted["model_ratio"] = ratioAny
	}
	if len(completionRatioMap) > 0 {
		compAny := make(map[string]any, len(completionRatioMap))
		for k, v := range completionRatioMap {
			compAny[k] = v
		}
		converted["completion_ratio"] = compAny
	}
	if len(modelPriceMap) > 0 {
		priceAny := make(map[string]any, len(modelPriceMap))
		for k, v := range modelPriceMap {
			priceAny[k] = v
		}
		converted["model_price"] = priceAny
	}

	return ratioPayload{Ratios: converted, BillingExpr: billingExprMap, BillingMode: billingModeMap}, 0, ""
}

// normalizeBillingMaps 丢掉空表达式，并给缺 billing_mode 的条目补 tiered_expr
// （有表达式即代表该模型按表达式计费）。
func normalizeBillingMaps(exprMap, modeMap map[string]string) (map[string]string, map[string]string) {
	expr := make(map[string]string, len(exprMap))
	mode := make(map[string]string, len(exprMap))
	for rawModel, rawExpr := range exprMap {
		model := strings.TrimSpace(rawModel)
		value := strings.TrimSpace(rawExpr)
		if model == "" || value == "" {
			continue
		}
		expr[model] = value
		if m := strings.TrimSpace(modeMap[rawModel]); m != "" {
			mode[model] = m
		} else {
			mode[model] = billing_setting.BillingModeTieredExpr
		}
	}
	return expr, mode
}

// sanitizeRatioData 过滤 ratioTypes 之外的字段与其中的非数字条目
// （例如上游把计费表达式写进了 model_ratio），只把数值倍率交给前端做 diff。
// 返回过滤后的 map 与被丢弃的非数字条目数；某个倍率类型被清空时连同该 key 一起删除。
func sanitizeRatioData(data map[string]any) (map[string]any, int) {
	dropped := 0
	for key := range data {
		if !slices.Contains(ratioTypes, key) {
			delete(data, key)
		}
	}
	for _, ratioType := range ratioTypes {
		raw, ok := data[ratioType]
		if !ok {
			continue
		}
		items, ok := raw.(map[string]any)
		if !ok {
			delete(data, ratioType)
			dropped++
			continue
		}
		for name, value := range items {
			number, isNumber := value.(float64)
			if !isNumber || math.IsNaN(number) || math.IsInf(number, 0) {
				delete(items, name)
				dropped++
			}
		}
		if len(items) == 0 {
			delete(data, ratioType)
		}
	}
	return data, dropped
}

func FetchUpstreamRatios(c *gin.Context) {
	var req dto.UpstreamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.SysError("failed to bind upstream request: " + err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "请求参数格式错误"})
		return
	}

	if req.Timeout <= 0 {
		req.Timeout = defaultTimeoutSeconds
	}

	var upstreams []dto.UpstreamDTO

	if len(req.Upstreams) > 0 {
		for _, u := range req.Upstreams {
			if strings.HasPrefix(u.BaseURL, "http") {
				if u.Endpoint == "" {
					u.Endpoint = defaultEndpoint
				}
				u.BaseURL = strings.TrimRight(u.BaseURL, "/")
				upstreams = append(upstreams, u)
			}
		}
	} else if len(req.ChannelIDs) > 0 {
		intIds := make([]int, 0, len(req.ChannelIDs))
		for _, id64 := range req.ChannelIDs {
			intIds = append(intIds, int(id64))
		}
		dbChannels, err := model.GetChannelsByIds(intIds)
		if err != nil {
			logger.LogError(c.Request.Context(), "failed to query channels: "+err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "查询渠道失败"})
			return
		}
		for _, ch := range dbChannels {
			if base := ch.GetBaseURL(); strings.HasPrefix(base, "http") {
				upstreams = append(upstreams, dto.UpstreamDTO{
					ID:       ch.Id,
					Name:     ch.Name,
					BaseURL:  strings.TrimRight(base, "/"),
					Endpoint: "",
				})
			}
		}
	}

	if len(upstreams) == 0 {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "无有效上游渠道"})
		return
	}

	var wg sync.WaitGroup
	ch := make(chan upstreamResult, len(upstreams))

	sem := make(chan struct{}, maxConcurrentFetches)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: 1 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
	if common.TLSInsecureSkipVerify {
		transport.TLSClientConfig = common.InsecureTLSConfig
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		// 对 github.io 优先尝试 IPv4，失败则回退 IPv6
		if strings.HasSuffix(host, "github.io") {
			if conn, err := dialer.DialContext(ctx, "tcp4", addr); err == nil {
				return conn, nil
			}
			return dialer.DialContext(ctx, "tcp6", addr)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	client := &http.Client{Transport: transport}

	for _, chn := range upstreams {
		wg.Add(1)
		go func(chItem dto.UpstreamDTO) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			isOpenRouter := chItem.Endpoint == "openrouter"

			endpoint := chItem.Endpoint
			var fullURL string
			if isOpenRouter {
				fullURL = chItem.BaseURL + "/v1/models"
			} else if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
				fullURL = endpoint
			} else {
				if endpoint == "" {
					endpoint = defaultEndpoint
				} else if !strings.HasPrefix(endpoint, "/") {
					endpoint = "/" + endpoint
				}
				fullURL = chItem.BaseURL + endpoint
			}
			isModelsDev := isModelsDevAPIEndpoint(fullURL)

			uniqueName := chItem.Name
			if chItem.ID != 0 {
				uniqueName = fmt.Sprintf("%s(%d)", chItem.Name, chItem.ID)
			}

			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(req.Timeout)*time.Second)
			defer cancel()

			httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
			if err != nil {
				logger.LogWarn(c.Request.Context(), "build request failed: "+err.Error())
				ch <- upstreamResult{Name: uniqueName, Err: err.Error()}
				return
			}

			// OpenRouter requires Bearer token auth
			if isOpenRouter && chItem.ID != 0 {
				dbCh, err := model.GetChannelById(chItem.ID, true)
				if err != nil {
					ch <- upstreamResult{Name: uniqueName, Err: "failed to get channel key: " + err.Error()}
					return
				}
				key, _, apiErr := dbCh.GetNextEnabledKey()
				if apiErr != nil {
					ch <- upstreamResult{Name: uniqueName, Err: "failed to get enabled channel key: " + apiErr.Error()}
					return
				}
				if strings.TrimSpace(key) == "" {
					ch <- upstreamResult{Name: uniqueName, Err: "no API key configured for this channel"}
					return
				}
				httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
			} else if isOpenRouter {
				ch <- upstreamResult{Name: uniqueName, Err: "OpenRouter requires a valid channel with API key"}
				return
			}

			// 简单重试：最多 3 次，指数退避
			var resp *http.Response
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				resp, lastErr = client.Do(httpReq)
				if lastErr == nil {
					break
				}
				time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
			}
			if lastErr != nil {
				logger.LogWarn(c.Request.Context(), "http error on "+chItem.Name+": "+lastErr.Error())
				ch <- upstreamResult{Name: uniqueName, Err: lastErr.Error()}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				logger.LogWarn(c.Request.Context(), "non-200 from "+chItem.Name+": "+resp.Status)
				ch <- upstreamResult{Name: uniqueName, Err: resp.Status}
				return
			}

			// Content-Type 和响应体大小校验
			if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "application/json") {
				logger.LogWarn(c.Request.Context(), "unexpected content-type from "+chItem.Name+": "+ct)
			}
			limited := io.LimitReader(resp.Body, maxRatioConfigBytes)
			bodyBytes, err := io.ReadAll(limited)
			if err != nil {
				logger.LogWarn(c.Request.Context(), "read response failed from "+chItem.Name+": "+err.Error())
				ch <- upstreamResult{Name: uniqueName, Err: err.Error()}
				return
			}

			// type3: OpenRouter /v1/models -> convert per-token pricing to ratios
			if isOpenRouter {
				converted, err := convertOpenRouterToRatioData(bytes.NewReader(bodyBytes))
				if err != nil {
					logger.LogWarn(c.Request.Context(), "OpenRouter parse failed from "+chItem.Name+": "+err.Error())
					ch <- upstreamResult{Name: uniqueName, Err: err.Error()}
					return
				}
				ch <- upstreamResult{Name: uniqueName, Data: converted}
				return
			}

			// type4: models.dev /api.json -> convert provider model pricing to ratios
			if isModelsDev {
				converted, err := convertModelsDevToRatioData(bytes.NewReader(bodyBytes))
				if err != nil {
					logger.LogWarn(c.Request.Context(), "models.dev parse failed from "+chItem.Name+": "+err.Error())
					ch <- upstreamResult{Name: uniqueName, Err: err.Error()}
					return
				}
				ch <- upstreamResult{Name: uniqueName, Data: converted}
				return
			}

			// 兼容两种上游接口格式：
			//  type1: /api/ratio_config -> data 为 map[string]any，包含 model_ratio/completion_ratio/cache_ratio/model_price
			//  type2: /api/pricing      -> data 为 []Pricing 列表，需要转换为与 type1 相同的 map 格式
			var body struct {
				Success bool            `json:"success"`
				Data    json.RawMessage `json:"data"`
				Message string          `json:"message"`
			}

			if err := common.DecodeJson(bytes.NewReader(bodyBytes), &body); err != nil {
				logger.LogWarn(c.Request.Context(), "json decode failed from "+chItem.Name+": "+err.Error())
				ch <- upstreamResult{Name: uniqueName, Err: err.Error()}
				return
			}

			if !body.Success {
				ch <- upstreamResult{Name: uniqueName, Err: body.Message}
				return
			}

			// 解析上游 data：数值倍率与阶梯计费表达式分开处理，表达式不会被当倍率用。
			payload, dropped, parseErrMsg := parseRatioPayload(body.Data)
			if parseErrMsg != "" {
				logger.LogWarn(c.Request.Context(), "ratio sync unsupported payload from "+chItem.Name+": "+parseErrMsg)
				ch <- upstreamResult{Name: uniqueName, Err: parseErrMsg}
				return
			}
			if dropped > 0 {
				logger.LogWarn(c.Request.Context(), fmt.Sprintf("ratio sync dropped %d non-numeric entries from %s", dropped, chItem.Name))
			}
			if payload.hasBilling() {
				logger.LogInfo(c.Request.Context(), fmt.Sprintf("ratio sync got %d billing expressions from %s", len(payload.BillingExpr), chItem.Name))
			}
			ch <- upstreamResult{
				Name:        uniqueName,
				Data:        payload.Ratios,
				BillingExpr: payload.BillingExpr,
				BillingMode: payload.BillingMode,
			}
			return
		}(chn)
	}

	wg.Wait()
	close(ch)

	localData := ratio_setting.GetExposedData()

	var testResults []dto.TestResult
	var successfulChannels []struct {
		name string
		data map[string]any
	}

	billingChannels := make([]struct {
		Name        string            `json:"name"`
		BillingExpr map[string]string `json:"billing_expr"`
		BillingMode map[string]string `json:"billing_mode"`
	}, 0)

	for r := range ch {
		if r.Err != "" {
			testResults = append(testResults, dto.TestResult{
				Name:   r.Name,
				Status: "error",
				Error:  r.Err,
			})
			continue
		}
		testResults = append(testResults, dto.TestResult{
			Name:   r.Name,
			Status: "success",
		})
		if len(r.Data) > 0 {
			successfulChannels = append(successfulChannels, struct {
				name string
				data map[string]any
			}{name: r.Name, data: r.Data})
		}
		if len(r.BillingExpr) > 0 {
			billingChannels = append(billingChannels, struct {
				Name        string            `json:"name"`
				BillingExpr map[string]string `json:"billing_expr"`
				BillingMode map[string]string `json:"billing_mode"`
			}{Name: r.Name, BillingExpr: r.BillingExpr, BillingMode: r.BillingMode})
		}
	}

	differences := buildDifferences(localData, successfulChannels)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"differences":  differences,
			"test_results": testResults,
			"billing":      billingChannels,
		},
	})
}

func buildDifferences(localData map[string]any, successfulChannels []struct {
	name string
	data map[string]any
}) map[string]map[string]dto.DifferenceItem {
	differences := make(map[string]map[string]dto.DifferenceItem)

	allModels := make(map[string]struct{})

	for _, ratioType := range ratioTypes {
		if localRatioAny, ok := localData[ratioType]; ok {
			if localRatio, ok := localRatioAny.(map[string]float64); ok {
				for modelName := range localRatio {
					allModels[modelName] = struct{}{}
				}
			}
		}
	}

	for _, channel := range successfulChannels {
		for _, ratioType := range ratioTypes {
			if upstreamRatio, ok := channel.data[ratioType].(map[string]any); ok {
				for modelName := range upstreamRatio {
					allModels[modelName] = struct{}{}
				}
			}
		}
	}

	confidenceMap := make(map[string]map[string]bool)

	// 预处理阶段：检查pricing接口的可信度
	for _, channel := range successfulChannels {
		confidenceMap[channel.name] = make(map[string]bool)

		modelRatios, hasModelRatio := channel.data["model_ratio"].(map[string]any)
		completionRatios, hasCompletionRatio := channel.data["completion_ratio"].(map[string]any)

		if hasModelRatio && hasCompletionRatio {
			// 遍历所有模型，检查是否满足不可信条件
			for modelName := range allModels {
				// 默认为可信
				confidenceMap[channel.name][modelName] = true

				// 检查是否满足不可信条件：model_ratio为37.5且completion_ratio为1
				if modelRatioVal, ok := modelRatios[modelName]; ok {
					if completionRatioVal, ok := completionRatios[modelName]; ok {
						// 转换为float64进行比较
						if modelRatioFloat, ok := modelRatioVal.(float64); ok {
							if completionRatioFloat, ok := completionRatioVal.(float64); ok {
								if modelRatioFloat == 37.5 && completionRatioFloat == 1.0 {
									confidenceMap[channel.name][modelName] = false
								}
							}
						}
					}
				}
			}
		} else {
			// 如果不是从pricing接口获取的数据，则全部标记为可信
			for modelName := range allModels {
				confidenceMap[channel.name][modelName] = true
			}
		}
	}

	for modelName := range allModels {
		for _, ratioType := range ratioTypes {
			var localValue interface{} = nil
			if localRatioAny, ok := localData[ratioType]; ok {
				if localRatio, ok := localRatioAny.(map[string]float64); ok {
					if val, exists := localRatio[modelName]; exists {
						localValue = val
					}
				}
			}

			upstreamValues := make(map[string]interface{})
			confidenceValues := make(map[string]bool)
			hasUpstreamValue := false
			hasDifference := false

			for _, channel := range successfulChannels {
				var upstreamValue interface{} = nil

				if upstreamRatio, ok := channel.data[ratioType].(map[string]any); ok {
					if val, exists := upstreamRatio[modelName]; exists {
						upstreamValue = val
						hasUpstreamValue = true

						if localValue != nil && !valuesEqual(localValue, val) {
							hasDifference = true
						} else if valuesEqual(localValue, val) {
							upstreamValue = "same"
						}
					}
				}
				if upstreamValue == nil && localValue == nil {
					upstreamValue = "same"
				}

				if localValue == nil && upstreamValue != nil && upstreamValue != "same" {
					hasDifference = true
				}

				upstreamValues[channel.name] = upstreamValue

				confidenceValues[channel.name] = confidenceMap[channel.name][modelName]
			}

			shouldInclude := false

			if localValue != nil {
				if hasDifference {
					shouldInclude = true
				}
			} else {
				if hasUpstreamValue {
					shouldInclude = true
				}
			}

			if shouldInclude {
				if differences[modelName] == nil {
					differences[modelName] = make(map[string]dto.DifferenceItem)
				}
				differences[modelName][ratioType] = dto.DifferenceItem{
					Current:    localValue,
					Upstreams:  upstreamValues,
					Confidence: confidenceValues,
				}
			}
		}
	}

	channelHasDiff := make(map[string]bool)
	for _, ratioMap := range differences {
		for _, item := range ratioMap {
			for chName, val := range item.Upstreams {
				if val != nil && val != "same" {
					channelHasDiff[chName] = true
				}
			}
		}
	}

	for modelName, ratioMap := range differences {
		for ratioType, item := range ratioMap {
			for chName := range item.Upstreams {
				if !channelHasDiff[chName] {
					delete(item.Upstreams, chName)
					delete(item.Confidence, chName)
				}
			}

			allSame := true
			for _, v := range item.Upstreams {
				if v != "same" {
					allSame = false
					break
				}
			}
			if len(item.Upstreams) == 0 || allSame {
				delete(ratioMap, ratioType)
			} else {
				differences[modelName][ratioType] = item
			}
		}

		if len(ratioMap) == 0 {
			delete(differences, modelName)
		}
	}

	return differences
}

func roundRatioValue(value float64) float64 {
	return math.Round(value*1e6) / 1e6
}

func isModelsDevAPIEndpoint(rawURL string) bool {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if strings.ToLower(parsedURL.Hostname()) != modelsDevHost {
		return false
	}
	path := strings.TrimSuffix(parsedURL.Path, "/")
	if path == "" {
		path = "/"
	}
	return path == modelsDevPath
}

// convertOpenRouterToRatioData parses OpenRouter's /v1/models response and converts
// per-token USD pricing into the local ratio format.
// model_ratio = prompt_price_per_token * 1_000_000 * (USD / 1000)
//
//	since 1 ratio unit = $0.002/1K tokens and USD=500, the factor is 500_000
//
// completion_ratio = completion_price / prompt_price (output/input multiplier)
func convertOpenRouterToRatioData(reader io.Reader) (map[string]any, error) {
	var orResp struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt         string `json:"prompt"`
				Completion     string `json:"completion"`
				InputCacheRead string `json:"input_cache_read"`
			} `json:"pricing"`
		} `json:"data"`
	}

	if err := common.DecodeJson(reader, &orResp); err != nil {
		return nil, fmt.Errorf("failed to decode OpenRouter response: %w", err)
	}

	modelRatioMap := make(map[string]any)
	completionRatioMap := make(map[string]any)
	cacheRatioMap := make(map[string]any)

	for _, m := range orResp.Data {
		promptPrice, promptErr := strconv.ParseFloat(m.Pricing.Prompt, 64)
		completionPrice, compErr := strconv.ParseFloat(m.Pricing.Completion, 64)

		if promptErr != nil && compErr != nil {
			// Both unparseable — skip this model
			continue
		}

		// Treat parse errors as 0
		if promptErr != nil {
			promptPrice = 0
		}
		if compErr != nil {
			completionPrice = 0
		}

		// Negative values are sentinel values (e.g., -1 for dynamic/variable pricing) — skip
		if promptPrice < 0 || completionPrice < 0 {
			continue
		}

		if promptPrice == 0 && completionPrice == 0 {
			// Free model
			modelRatioMap[m.ID] = 0.0
			continue
		}
		if promptPrice <= 0 {
			// No meaningful prompt baseline, cannot derive ratios safely.
			continue
		}

		// Normal case: promptPrice > 0
		ratio := promptPrice * 1000 * ratio_setting.USD
		ratio = roundRatioValue(ratio)
		modelRatioMap[m.ID] = ratio

		compRatio := completionPrice / promptPrice
		compRatio = roundRatioValue(compRatio)
		completionRatioMap[m.ID] = compRatio

		// Convert input_cache_read to cache_ratio (= cache_read_price / prompt_price)
		if m.Pricing.InputCacheRead != "" {
			if cachePrice, err := strconv.ParseFloat(m.Pricing.InputCacheRead, 64); err == nil && cachePrice >= 0 {
				cacheRatio := cachePrice / promptPrice
				cacheRatio = roundRatioValue(cacheRatio)
				cacheRatioMap[m.ID] = cacheRatio
			}
		}
	}

	converted := make(map[string]any)
	if len(modelRatioMap) > 0 {
		converted["model_ratio"] = modelRatioMap
	}
	if len(completionRatioMap) > 0 {
		converted["completion_ratio"] = completionRatioMap
	}
	if len(cacheRatioMap) > 0 {
		converted["cache_ratio"] = cacheRatioMap
	}

	return converted, nil
}

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Cost modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input     *float64 `json:"input"`
	Output    *float64 `json:"output"`
	CacheRead *float64 `json:"cache_read"`
}

type modelsDevCandidate struct {
	Provider  string
	Input     float64
	Output    *float64
	CacheRead *float64
}

func cloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func isValidNonNegativeCost(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	return v >= 0
}

func buildModelsDevCandidate(provider string, cost modelsDevCost) (modelsDevCandidate, bool) {
	if cost.Input == nil {
		return modelsDevCandidate{}, false
	}

	input := *cost.Input
	if !isValidNonNegativeCost(input) {
		return modelsDevCandidate{}, false
	}

	var output *float64
	if cost.Output != nil {
		if !isValidNonNegativeCost(*cost.Output) {
			return modelsDevCandidate{}, false
		}
		output = cloneFloatPtr(cost.Output)
	}

	// input=0/output>0 cannot be transformed into local ratio.
	if input == 0 && output != nil && *output > 0 {
		return modelsDevCandidate{}, false
	}

	var cacheRead *float64
	if cost.CacheRead != nil && isValidNonNegativeCost(*cost.CacheRead) {
		cacheRead = cloneFloatPtr(cost.CacheRead)
	}

	return modelsDevCandidate{
		Provider:  provider,
		Input:     input,
		Output:    output,
		CacheRead: cacheRead,
	}, true
}

func shouldReplaceModelsDevCandidate(current, next modelsDevCandidate) bool {
	currentNonZero := current.Input > 0
	nextNonZero := next.Input > 0
	if currentNonZero != nextNonZero {
		// Prefer non-zero pricing data; this matches "cheapest non-zero" conflict policy.
		return nextNonZero
	}
	if nextNonZero && !nearlyEqual(next.Input, current.Input) {
		return next.Input < current.Input
	}
	// Stable tie-breaker for deterministic result.
	return next.Provider < current.Provider
}

// convertModelsDevToRatioData parses models.dev /api.json and converts
// provider pricing metadata into local ratio format.
// models.dev costs are USD per 1M tokens:
//
//	model_ratio = input_cost_per_1M / 2
//	completion_ratio = output_cost / input_cost
//	cache_ratio = cache_read_cost / input_cost
//
// Duplicate model keys across providers are resolved by selecting the
// cheapest non-zero input cost. If only zero-priced candidates exist,
// a zero ratio is kept.
func convertModelsDevToRatioData(reader io.Reader) (map[string]any, error) {
	var upstreamData map[string]modelsDevProvider
	if err := common.DecodeJson(reader, &upstreamData); err != nil {
		return nil, fmt.Errorf("failed to decode models.dev response: %w", err)
	}
	if len(upstreamData) == 0 {
		return nil, fmt.Errorf("empty models.dev response")
	}

	providers := make([]string, 0, len(upstreamData))
	for provider := range upstreamData {
		providers = append(providers, provider)
	}
	sort.Strings(providers)

	selectedCandidates := make(map[string]modelsDevCandidate)
	for _, provider := range providers {
		providerData := upstreamData[provider]
		if len(providerData.Models) == 0 {
			continue
		}

		modelNames := make([]string, 0, len(providerData.Models))
		for modelName := range providerData.Models {
			modelNames = append(modelNames, modelName)
		}
		sort.Strings(modelNames)

		for _, modelName := range modelNames {
			candidate, ok := buildModelsDevCandidate(provider, providerData.Models[modelName].Cost)
			if !ok {
				continue
			}
			current, exists := selectedCandidates[modelName]
			if !exists || shouldReplaceModelsDevCandidate(current, candidate) {
				selectedCandidates[modelName] = candidate
			}
		}
	}

	if len(selectedCandidates) == 0 {
		return nil, fmt.Errorf("no valid models.dev pricing entries found")
	}

	modelRatioMap := make(map[string]any)
	completionRatioMap := make(map[string]any)
	cacheRatioMap := make(map[string]any)

	for modelName, candidate := range selectedCandidates {
		if candidate.Input == 0 {
			modelRatioMap[modelName] = 0.0
			continue
		}

		modelRatio := candidate.Input * float64(ratio_setting.USD) / modelsDevInputCostRatioBase
		modelRatioMap[modelName] = roundRatioValue(modelRatio)

		if candidate.Output != nil {
			completionRatio := *candidate.Output / candidate.Input
			completionRatioMap[modelName] = roundRatioValue(completionRatio)
		}

		if candidate.CacheRead != nil {
			cacheRatio := *candidate.CacheRead / candidate.Input
			cacheRatioMap[modelName] = roundRatioValue(cacheRatio)
		}
	}

	converted := make(map[string]any)
	if len(modelRatioMap) > 0 {
		converted["model_ratio"] = modelRatioMap
	}
	if len(completionRatioMap) > 0 {
		converted["completion_ratio"] = completionRatioMap
	}
	if len(cacheRatioMap) > 0 {
		converted["cache_ratio"] = cacheRatioMap
	}
	return converted, nil
}

func GetSyncableChannels(c *gin.Context) {
	channels, err := model.GetAllChannels(0, 0, true, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	var syncableChannels []dto.SyncableChannel
	for _, channel := range channels {
		if channel.GetBaseURL() != "" {
			syncableChannels = append(syncableChannels, dto.SyncableChannel{
				ID:      channel.Id,
				Name:    channel.Name,
				BaseURL: channel.GetBaseURL(),
				Status:  channel.Status,
				Type:    channel.Type,
			})
		}
	}

	syncableChannels = append(syncableChannels, dto.SyncableChannel{
		ID:      officialRatioPresetID,
		Name:    officialRatioPresetName,
		BaseURL: officialRatioPresetBaseURL,
		Status:  1,
	})

	syncableChannels = append(syncableChannels, dto.SyncableChannel{
		ID:      modelsDevPresetID,
		Name:    modelsDevPresetName,
		BaseURL: modelsDevPresetBaseURL,
		Status:  1,
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    syncableChannels,
	})
}

// ImportBillingSettings 把上游的阶梯计费表达式合并进本机的 billing_setting。
// 每条表达式先跑 smoke test，不通过的跳过并回报，绝不写进配置。
func ImportBillingSettings(c *gin.Context) {
	var req struct {
		BillingExpr map[string]string `json:"billing_expr"`
		BillingMode map[string]string `json:"billing_mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "请求参数格式错误"})
		return
	}
	if len(req.BillingExpr) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "没有可导入的计费表达式"})
		return
	}

	currentExpr := map[string]string{}
	currentMode := map[string]string{}
	if raw, ok := common.OptionMap["billing_setting.billing_expr"]; ok {
		if err := common.UnmarshalJsonStr(raw, &currentExpr); err != nil {
			logger.LogWarn(c.Request.Context(), "failed to parse current billing_expr option: "+err.Error())
			currentExpr = map[string]string{}
		}
	}
	if raw, ok := common.OptionMap["billing_setting.billing_mode"]; ok {
		if err := common.UnmarshalJsonStr(raw, &currentMode); err != nil {
			logger.LogWarn(c.Request.Context(), "failed to parse current billing_mode option: "+err.Error())
			currentMode = map[string]string{}
		}
	}

	mergedExpr, mergedMode, imported, changed, skipped := mergeBillingSettings(currentExpr, currentMode, req.BillingExpr, req.BillingMode)
	currentExpr, currentMode = mergedExpr, mergedMode

	if imported == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "所有表达式都未通过校验，没有写入任何配置",
			"data":    gin.H{"imported": 0, "changed": 0, "total": len(currentExpr), "skipped": skipped},
		})
		return
	}

	exprJSON, err := common.Marshal(currentExpr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "序列化 billing_expr 失败"})
		return
	}
	modeJSON, err := common.Marshal(currentMode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "序列化 billing_mode 失败"})
		return
	}
	if err := model.UpdateOption("billing_setting.billing_expr", string(exprJSON)); err != nil {
		logger.LogError(c.Request.Context(), "failed to save billing_expr: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "保存 billing_expr 失败"})
		return
	}
	if err := model.UpdateOption("billing_setting.billing_mode", string(modeJSON)); err != nil {
		logger.LogError(c.Request.Context(), "failed to save billing_mode: "+err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "保存 billing_mode 失败"})
		return
	}

	logger.LogInfo(c.Request.Context(), fmt.Sprintf("imported %d billing expressions (%d changed), skipped %d", imported, changed, len(skipped)))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"imported": imported,
			"changed":  changed,
			"total":    len(currentExpr),
			"skipped":  skipped,
		},
	})
}

// billingSkippedItem 是一条没通过校验、没有写入配置的表达式。
type billingSkippedItem struct {
	Model string `json:"model"`
	Error string `json:"error"`
}

// mergeBillingSettings 把待导入的表达式合并进本机配置：
// 逐条先跑 billing_setting.SmokeTestExpr，失败的跳过并回报，成功的覆盖同名模型。
// 返回合并后的 expr/mode、导入条数、与现值不同的条数、被跳过的条目。
func mergeBillingSettings(currentExpr, currentMode, newExpr, newMode map[string]string) (
	map[string]string, map[string]string, int, int, []billingSkippedItem,
) {
	if currentExpr == nil {
		currentExpr = map[string]string{}
	}
	if currentMode == nil {
		currentMode = map[string]string{}
	}
	// 先复制一份再合并，调用方传进来的 map 保持不被就地改写。
	mergedExpr := make(map[string]string, len(currentExpr)+len(newExpr))
	for k, v := range currentExpr {
		mergedExpr[k] = v
	}
	mergedMode := make(map[string]string, len(currentMode)+len(newMode))
	for k, v := range currentMode {
		mergedMode[k] = v
	}
	skipped := make([]billingSkippedItem, 0)
	imported := 0
	changed := 0

	for rawModel, rawExpr := range newExpr {
		model := strings.TrimSpace(rawModel)
		expr := strings.TrimSpace(rawExpr)
		if model == "" || expr == "" {
			continue
		}
		if err := billing_setting.SmokeTestExpr(expr); err != nil {
			skipped = append(skipped, billingSkippedItem{Model: model, Error: err.Error()})
			continue
		}
		mode := strings.TrimSpace(newMode[rawModel])
		if mode == "" {
			mode = billing_setting.BillingModeTieredExpr
		}
		if mergedExpr[model] != expr || mergedMode[model] != mode {
			changed++
		}
		mergedExpr[model] = expr
		mergedMode[model] = mode
		imported++
	}
	return mergedExpr, mergedMode, imported, changed, skipped
}
