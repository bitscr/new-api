package controller

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel/mistral"
	"github.com/QuantumNous/new-api/relay/channel/typesafe"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func relayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if info.RelayFormat == types.RelayFormatTypeSafe {
		return relay.TypeSafeHelper(c, info)
	}
	if info.RelayFormat == types.RelayFormatMistralNative || info.RelayFormat == types.RelayFormatMistralRealtime {
		return relay.MistralNativeHelper(c, info)
	}
	var err *types.NewAPIError
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		err = relay.ImageHelper(c, info)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, info)
	case relayconstant.RelayModeRerank:
		err = relay.RerankHelper(c, info)
	case relayconstant.RelayModeEmbeddings:
		err = relay.EmbeddingHelper(c, info)
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		err = relay.ResponsesHelper(c, info)
	case relayconstant.RelayModeOpenAILocalSearch:
		err = relay.OpenAILocalSearchHelper(c, info)
	default:
		err = relay.TextHelper(c, info)
	}
	return err
}

func geminiRelayHandler(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	var err *types.NewAPIError
	if strings.Contains(c.Request.URL.Path, "embed") {
		err = relay.GeminiEmbeddingHandler(c, info)
	} else {
		err = relay.GeminiHelper(c, info)
	}
	return err
}

func Relay(c *gin.Context, relayFormat types.RelayFormat) {
	// Reject unsupported protocols before any WebSocket upgrade or pre-charge.
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeTypeSafe && relayFormat != types.RelayFormatTypeSafe {
		c.JSON(http.StatusBadRequest, gin.H{"error": types.OpenAIError{
			Message: "TypeSafe only supports POST /v1/systemone",
			Type:    "invalid_request_error", Code: types.ErrorCodeInvalidRequest,
		}})
		return
	}
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeMistral && mistral.UsesNativeProtocol(c) {
		relayFormat = types.RelayFormatMistralNative
		if c.Request.URL.Path == mistral.RealtimePath {
			relayFormat = types.RelayFormatMistralRealtime
		}
	}

	requestId := c.GetString(common.RequestIdKey)
	//group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	//originalModel := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)

	var (
		newAPIError *types.NewAPIError
		ws          *websocket.Conn
	)

	if relayFormat == types.RelayFormatOpenAIRealtime {
		if common.GetContextKeyInt(c, constant.ContextKeyChannelType) == constant.ChannelTypeOpenAI && dto.IsRealtimeBetaRequest(c.Request.Header) {
			c.JSON(http.StatusBadRequest, gin.H{"error": types.OpenAIError{Type: "invalid_request_error", Code: "realtime_beta_not_supported", Param: "OpenAI-Beta", Message: "Realtime Beta is no longer supported; remove the Beta header/subprotocol and migrate the client to Realtime GA"}})
			return
		}
		var err error
		ws, err = upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			helper.WssError(c, ws, types.NewError(err, types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry()).ToOpenAIError())
			return
		}
		defer ws.Close()
	}

	defer func() {
		if newAPIError != nil {
			logger.LogError(c, fmt.Sprintf("relay error: %s", newAPIError.Error()))
			if relayFormat == types.RelayFormatTypeSafe && typesafe.WriteUpstreamError(c, newAPIError) {
				return
			}
			if (relayFormat == types.RelayFormatMistralNative || relayFormat == types.RelayFormatMistralRealtime) && c.Writer.Written() {
				return // Native HTTP/SSE/WS errors have already been relayed verbatim.
			}
			if !isChannelDailySuccessLimitError(newAPIError) && !service.IsChannelRPMLimitError(newAPIError) {
				newAPIError.SetMessage(common.MessageWithRequestId(newAPIError.Error(), requestId))
			}
			switch relayFormat {
			case types.RelayFormatOpenAIRealtime:
				helper.WssError(c, ws, newAPIError.ToOpenAIError())
			case types.RelayFormatClaude:
				c.JSON(newAPIError.StatusCode, gin.H{
					"type":  "error",
					"error": newAPIError.ToClaudeError(),
				})
			default:
				c.JSON(newAPIError.StatusCode, gin.H{
					"error": newAPIError.ToOpenAIError(),
				})
			}
		}
	}()

	request, err := helper.GetAndValidateRequest(c, relayFormat)
	if err != nil {
		// Map "request body too large" to 413 so clients can handle it correctly
		if common.IsRequestBodyTooLargeError(err) || errors.Is(err, common.ErrRequestBodyTooLarge) {
			newAPIError = types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
		} else {
			newAPIError = types.NewError(err, types.ErrorCodeInvalidRequest)
		}
		return
	}

	relayInfo, err := relaycommon.GenRelayInfo(c, relayFormat, request, ws)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeGenRelayInfoFailed)
		return
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	needCountToken := constant.CountToken
	// Avoid building huge CombineText (strings.Join) when token counting and sensitive check are both disabled.
	var meta *types.TokenCountMeta
	if needSensitiveCheck || needCountToken {
		meta = request.GetTokenCountMeta()
	} else {
		meta = fastTokenCountMetaForPricing(request)
	}

	if needSensitiveCheck && meta != nil {
		contains, words := service.CheckSensitiveText(meta.CombineText)
		if contains {
			logger.LogWarn(c, fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", ")))
			newAPIError = types.NewError(err, types.ErrorCodeSensitiveWordsDetected)
			return
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, relayInfo)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeCountTokenFailed)
		return
	}

	relayInfo.SetEstimatePromptTokens(tokens)

	newAPIError = service.CheckProbeGuard(c, relayInfo)
	if newAPIError != nil {
		return
	}

	priceData, err := helper.ModelPriceHelper(c, relayInfo, tokens, meta)
	if err != nil {
		newAPIError = types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
		return
	}

	// common.SetContextKey(c, constant.ContextKeyTokenCountMeta, meta)

	if priceData.FreeModel {
		logger.LogInfo(c, fmt.Sprintf("模型 %s 免费，跳过预扣费", relayInfo.OriginModelName))
	} else {
		newAPIError = service.PreConsumeBilling(c, priceData.QuotaToPreConsume, relayInfo)
		if newAPIError != nil {
			return
		}
	}

	defer func() {
		// Only return quota if downstream failed and quota was actually pre-consumed
		if newAPIError != nil {
			newAPIError = service.NormalizeViolationFeeErrorForRelay(relayInfo, newAPIError)
			service.RefundBilling(c, relayInfo)
			service.ChargeViolationFeeIfNeeded(c, relayInfo, newAPIError)
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}
	relayInfo.RetryIndex = 0
	relayInfo.LastError = nil
	// Commit only the final outcome of each (group, model, channel) in this request.
	// A failed attempt must not consume the feedback slot of a later success.
	defer flushAutoModelFeedback(c)

	for retryParam.GetRetry() <= retryParam.GetEffectiveRetryTimes() {
		if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil {
			newAPIError = canceledErr
			break
		}
		relayInfo.RetryIndex = retryParam.GetRetry()
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			// auto 路由：当前模型渠道耗尽时，切换到下一个候选模型再尝试
			exhaustedModel := relayInfo.OriginModelName
			if trySwitchAutoModel(c, relayInfo, retryParam, tokens, meta) {
				logger.LogInfo(c, fmt.Sprintf("auto model: 候选 %s 的渠道已耗尽，切换候选 %s", exhaustedModel, relayInfo.OriginModelName))
				continue
			}
			// 已经失败过至少一次才走到这里，说明不是"没有渠道"，而是候选渠道都被排除了
			// （例如同一渠道试满上限）。把真实的上游报错返回，别用"可用渠道不存在"误导排查。
			if retryParam.GetRetry() > 0 && relayInfo.LastError != nil {
				newAPIError = relayInfo.LastError
				break
			}
			newAPIError = channelErr
			break
		}

		reservation, reserveErr := reserveChannelDailySuccess(channel)
		if reserveErr != nil {
			if isChannelDailySuccessLimitError(reserveErr) && shouldSkipDailyLimitedChannel(c, channel) {
				continue
			}
			newAPIError = reserveErr
			break
		}

		retryParam.SetEffectiveRetryTimesFromChannel(channel)
		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			// Ensure consistent 413 for oversized bodies even when error occurs later (e.g., retry path)
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusRequestEntityTooLarge, types.ErrOptionWithSkipRetry())
			} else {
				newAPIError = types.NewErrorWithStatusCode(bodyErr, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			model.ReleaseChannelDailySuccess(reservation)
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		relayInfo.BeginAttempt()
		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			newAPIError = relay.WssHelper(c, relayInfo)
		case types.RelayFormatClaude:
			newAPIError = relay.ClaudeHelper(c, relayInfo)
		case types.RelayFormatGemini:
			newAPIError = geminiRelayHandler(c, relayInfo)
		default:
			newAPIError = relayHandler(c, relayInfo)
		}
		relayInfo.EndAttempt()

		if newAPIError == nil {
			if autoModelRequestContextError(c) != nil {
				// Some stream adaptors return nil on client disconnect. Do not
				// classify a partial answer as healthy or as an upstream failure.
				relayInfo.LastError = nil
				return
			}
			// 200 但没有有效回答（正文为空，或正文只是上游网关的告警横幅）不算成功：
			// 旧逻辑把它当"快速成功"，auto 于是越选越多这个渠道的这个模型，
			// 而调用方每次都拿到假答案（实测渠道 #5 近 7 天 10 次全部如此）。
			if unusable, reason := relayInfo.ClientAnswerUnusable(); unusable {
				logger.LogInfo(c, fmt.Sprintf("auto model: 候选 %s 没有给出有效回答（%s），不计成功", relayInfo.OriginModelName, reason))
				recordAutoModelUnusableAnswer(c, relayInfo, reason)
			} else {
				recordAutoModelFeedback(c, relayInfo, true)
			}
			relayInfo.LastError = nil
			return
		}
		model.ReleaseChannelDailySuccess(reservation)
		if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil {
			// The client disappearing is not evidence against this upstream. Keep
			// earlier real failures queued, but do not penalize or retry this attempt.
			newAPIError = canceledErr
			break
		}
		if service.IsChannelRPMLimitError(newAPIError) {
			relayInfo.LastError = newAPIError
			if shouldSkipRPMLimitedChannel(c, channel) {
				continue
			}
			break
		}

		newAPIError = service.NormalizeViolationFeeErrorForRelay(relayInfo, newAPIError)
		relayInfo.LastError = newAPIError

		processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)
		recordAutoModelFeedback(c, relayInfo, false)

		if !shouldRetry(c, newAPIError, retryParam.GetRemainingRetryTimes()) {
			// auto 路由：上游明确表示服务不了当前候选（404 模型不支持、400 参数不支持等）时，
			// 换下一个候选模型再试。否则客户端只会收到 404/400 并直接停止
			// （实测：上游 404 被原样返回，客户端报 model_not_found）。
			if autoModelSwitchableError(newAPIError) &&
				c.GetInt("auto_model_hard_switches") < maxAutoModelHardSwitches {
				oldModel := relayInfo.OriginModelName
				if trySwitchAutoModel(c, relayInfo, retryParam, tokens, meta) {
					c.Set("auto_model_hard_switches", c.GetInt("auto_model_hard_switches")+1)
					logger.LogInfo(c, fmt.Sprintf("auto model: 候选 %s 被上游拒绝（status %d），切换候选 %s",
						oldModel, newAPIError.StatusCode, relayInfo.OriginModelName))
					continue
				}
			}
			break
		}
		retryParam.IncreaseRetry()
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}
}

var upgrader = websocket.Upgrader{
	Subprotocols: []string{"realtime"}, // WS 握手支持的协议，如果有使用 Sec-WebSocket-Protocol，则必须在此声明对应的 Protocol TODO add other protocol
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域
	},
}

func addUsedChannel(c *gin.Context, channelId int) {
	useChannel := c.GetStringSlice("use_channel")
	useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
	c.Set("use_channel", useChannel)
}

func newChannelDailySuccessLimitError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(
		model.ErrChannelDailySuccessLimitExceeded,
		types.ErrorCodeChannelDailySuccessLimitExceeded,
		http.StatusTooManyRequests,
		types.ErrOptionWithSkipRetry(),
		types.ErrOptionWithNoRecordErrorLog(),
	)
}

func reserveChannelDailySuccess(channel *model.Channel) (*model.ChannelDailySuccessReservation, *types.NewAPIError) {
	reservation, err := model.ReserveChannelDailySuccess(channel)
	if err == nil {
		return reservation, nil
	}
	if errors.Is(err, model.ErrChannelDailySuccessLimitExceeded) {
		return nil, newChannelDailySuccessLimitError()
	}
	return nil, types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
}

func isChannelDailySuccessLimitError(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, model.ErrChannelDailySuccessLimitExceeded) || err.GetErrorCode() == types.ErrorCodeChannelDailySuccessLimitExceeded
}

func isSpecificChannelRequest(c *gin.Context) bool {
	_, ok := c.Get(string(constant.ContextKeyTokenSpecificChannelId))
	return ok
}

func shouldSkipDailyLimitedChannel(c *gin.Context, channel *model.Channel) bool {
	if channel == nil || channel.Id <= 0 || isSpecificChannelRequest(c) {
		return false
	}
	service.MarkChannelDailySuccessLimitSkipped(c, channel.Id)
	logger.LogInfo(c, fmt.Sprintf("channel #%d reached daily success limit, trying another channel", channel.Id))
	return true
}

func shouldSkipRPMLimitedChannel(c *gin.Context, channel *model.Channel) bool {
	if channel == nil || channel.Id <= 0 || isSpecificChannelRequest(c) {
		return false
	}
	service.MarkChannelRPMLimitSkipped(c, channel.Id)
	logger.LogInfo(c, fmt.Sprintf("channel #%d reached RPM protection limit, trying another channel", channel.Id))
	return true
}

func fastTokenCountMetaForPricing(request dto.Request) *types.TokenCountMeta {
	if request == nil {
		return &types.TokenCountMeta{}
	}
	meta := &types.TokenCountMeta{
		TokenType: types.TokenTypeTokenizer,
	}
	switch r := request.(type) {
	case *dto.MistralNativeRequest:
		meta.MaxTokens = r.MaxTokens
	case *dto.GeneralOpenAIRequest:
		maxCompletionTokens := lo.FromPtrOr(r.MaxCompletionTokens, uint(0))
		maxTokens := lo.FromPtrOr(r.MaxTokens, uint(0))
		if maxCompletionTokens > maxTokens {
			meta.MaxTokens = int(maxCompletionTokens)
		} else {
			meta.MaxTokens = int(maxTokens)
		}
	case *dto.OpenAIResponsesRequest:
		meta.MaxTokens = int(lo.FromPtrOr(r.MaxOutputTokens, uint(0)))
	case *dto.ClaudeRequest:
		meta.MaxTokens = int(lo.FromPtr(r.MaxTokens))
	case *dto.ImageRequest:
		// Pricing for image requests depends on ImagePriceRatio; safe to compute even when CountToken is disabled.
		return r.GetTokenCountMeta()
	case *dto.OpenAILocalSearchRequest:
		return r.GetTokenCountMeta()
	default:
		// Best-effort: leave CombineText empty to avoid large allocations.
	}
	return meta
}

func getChannel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam) (*model.Channel, *types.NewAPIError) {
	if info.ChannelMeta == nil {
		autoBan := c.GetBool("auto_ban")
		autoBanInt := 1
		if !autoBan {
			autoBanInt = 0
		}
		channelId := c.GetInt("channel_id")
		if (!service.IsChannelDailySuccessLimitSkipped(c, channelId) && !service.IsChannelRPMLimitSkipped(c, channelId)) || isSpecificChannelRequest(c) {
			if channel, err := model.CacheGetChannel(channelId); err == nil && channel != nil {
				return channel, nil
			}
			if channel, err := model.GetChannelById(channelId, true); err == nil && channel != nil {
				return channel, nil
			}
			return &model.Channel{
				Id:      channelId,
				Type:    c.GetInt("channel_type"),
				Name:    c.GetString("channel_name"),
				AutoBan: &autoBanInt,
			}, nil
		}
	}
	channel, selectGroup, err := service.CacheGetRandomSatisfiedChannel(retryParam)

	info.PriceData.GroupRatioInfo = helper.HandleGroupRatio(c, info)

	if err != nil {
		return nil, types.NewError(fmt.Errorf("获取分组 %s 下模型 %s 的可用渠道失败（retry）: %s", selectGroup, info.OriginModelName, err.Error()), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if channel == nil {
		if service.HasChannelRPMLimitSkipped(c) {
			return nil, service.NewChannelRPMLimitError(service.ChannelRPMGroupLimitExceededMessage)
		}
		if service.HasChannelDailySuccessLimitSkipped(c) {
			return nil, newChannelDailySuccessLimitError()
		}
		return nil, types.NewError(fmt.Errorf("分组 %s 下模型 %s 的可用渠道不存在（retry）", selectGroup, info.OriginModelName), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}

	newAPIError := middleware.SetupContextForSelectedChannel(c, channel, info.OriginModelName)
	if newAPIError != nil {
		return nil, newAPIError
	}
	return channel, nil
}

func shouldRetry(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	if autoModelRequestContextError(c) != nil {
		return false
	}
	if openaiErr == nil {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if types.IsChannelError(openaiErr) {
		return true
	}
	if types.IsSkipRetryError(openaiErr) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	code := openaiErr.StatusCode
	if code >= 200 && code < 300 {
		return false
	}
	if code < 100 || code > 599 {
		return true
	}
	if operation_setting.IsAlwaysSkipRetryCode(openaiErr.GetErrorCode()) {
		return false
	}
	return operation_setting.ShouldRetryByStatusCode(code)
}

// isAutoModelRequest 判断当前请求是否是分组内虚拟 auto 模型路由的请求。
func isAutoModelRequest(c *gin.Context) bool {
	if c == nil {
		return false
	}
	_, ok := common.GetContextKey(c, constant.ContextKeyAutoModelClientName)
	return ok
}

// autoModelRequestContextError deliberately leaves non-auto behavior unchanged.
func autoModelRequestContextError(c *gin.Context) error {
	if !isAutoModelRequest(c) || c.Request == nil {
		return nil
	}
	return c.Request.Context().Err()
}

// autoModelCanceledRequestError preserves an already observed upstream error.
// The synthetic error is only needed when cancellation precedes any attempt.
func autoModelCanceledRequestError(c *gin.Context, current *types.NewAPIError) *types.NewAPIError {
	err := autoModelRequestContextError(c)
	if err == nil {
		return nil
	}
	if current != nil {
		return current
	}
	return types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, http.StatusRequestTimeout,
		types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
}

// trySwitchAutoModel 在“当前模型渠道耗尽”时把路由切换到下一个候选模型。
// 返回 true 表示已切换到下一模型，调用方应继续重试循环。
// 切换会更新 OriginModelName/ClientModelName/PriceData 并重置渠道已用列表，
// 使新模型拥有完整渠道池。
func trySwitchAutoModel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam, tokens int, meta *types.TokenCountMeta) bool {
	if !operation_setting.AutoModelEnabled || !isAutoModelRequest(c) || autoModelRequestContextError(c) != nil {
		return false
	}
	candidatesAny, ok := common.GetContextKey(c, constant.ContextKeyAutoModelCandidates)
	if !ok {
		return false
	}
	candidates, ok := candidatesAny.([]string)
	if !ok || len(candidates) == 0 {
		return false
	}
	index := common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex)
	if index < 0 || index >= len(candidates) {
		return false
	}
	next := index + 1
	// Treat the candidate snapshot as untrusted at the final model-switch boundary.
	// Middleware filtering must not be the only guard against token scope bypass.
	for next < len(candidates) && !service.IsAutoModelCandidateAuthorized(c, candidates[next]) {
		next++
	}
	if next >= len(candidates) {
		logger.LogInfo(c, fmt.Sprintf("auto model: 授权候选模型已全部尝试（%s）", strings.Join(candidates, " -> ")))
		return false
	}
	oldModel := info.OriginModelName
	newModel := candidates[next]

	info.OriginModelName = newModel
	info.ClientModelName = newModel
	c.Set("original_model", newModel)
	retryParam.ModelName = newModel

	// 重新计算新模型的报价，结算/日志按新模型定价
	priceData, err := helper.ModelPriceHelper(c, info, tokens, meta)
	if err != nil {
		logger.LogWarn(c, fmt.Sprintf("auto model switch 到 %s 计算价格失败，保持当前模型: %s", newModel, err.Error()))
		info.OriginModelName = oldModel
		info.ClientModelName = oldModel
		c.Set("original_model", oldModel)
		retryParam.ModelName = oldModel
		return false
	}
	info.PriceData = priceData

	// 换模型后渠道池重新开始（同模型已试渠道不阻塞新模型）
	c.Set("use_channel", make([]string, 0))
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, next)
	logger.LogInfo(c, fmt.Sprintf("auto model: 切换候选 %s -> %s", oldModel, newModel))
	return true
}

// recordAutoModelFeedback captures this attempt now; flushing later must not read
// mutable RelayInfo fields belonging to another model/channel or a later attempt.
func recordAutoModelFeedback(c *gin.Context, info *relaycommon.RelayInfo, success bool) {
	if !isAutoModelRequest(c) || info == nil || autoModelRequestContextError(c) != nil {
		return
	}
	queueAutoModelFeedback(c, info, autoModelFeedback{
		success:   success,
		latencyMS: observedAutoLatency(info, success),
		elapsedMS: observedAutoModelCooldownLatency(info, success),
	})
}

// An unusable HTTP 200 is the final failure of this combination, not a success.
func recordAutoModelUnusableAnswer(c *gin.Context, info *relaycommon.RelayInfo, reason string) {
	if !isAutoModelRequest(c) || info == nil || autoModelRequestContextError(c) != nil {
		return
	}
	queueAutoModelFeedback(c, info, autoModelFeedback{unusable: true, reason: reason})
}

type autoModelFeedbackKey struct {
	group     string
	modelName string
	channelID int
}

type autoModelFeedback struct {
	success   bool
	latencyMS int64
	elapsedMS int64 // cooldown signal: successful TTFB (or non-stream elapsed), failed elapsed
	unusable  bool
	reason    string
}

const autoModelFeedbackContextKey = "auto_model_feedback_pending"

func queueAutoModelFeedback(c *gin.Context, info *relaycommon.RelayInfo, feedback autoModelFeedback) {
	channelID := 0
	if info.ChannelMeta != nil {
		channelID = info.ChannelMeta.ChannelId
	}
	key := autoModelFeedbackKey{group: info.UsingGroup, modelName: info.OriginModelName, channelID: channelID}
	value, _ := c.Get(autoModelFeedbackContextKey)
	pending, _ := value.(map[autoModelFeedbackKey]autoModelFeedback)
	if pending == nil {
		pending = make(map[autoModelFeedbackKey]autoModelFeedback)
	}
	// Last attempt wins: many failed retries count once, and a later success is
	// allowed to replace an earlier failure (including its stale latency).
	pending[key] = feedback
	c.Set(autoModelFeedbackContextKey, pending)
}

func takeAutoModelFeedback(c *gin.Context) map[autoModelFeedbackKey]autoModelFeedback {
	if c == nil {
		return nil
	}
	value, _ := c.Get(autoModelFeedbackContextKey)
	pending, _ := value.(map[autoModelFeedbackKey]autoModelFeedback)
	c.Set(autoModelFeedbackContextKey, nil)
	return pending
}

func flushAutoModelFeedback(c *gin.Context) {
	for key, feedback := range takeAutoModelFeedback(c) {
		if feedback.unusable {
			service.RecordAutoModelUnusableAnswer(key.group, key.modelName, key.channelID, feedback.reason)
		} else {
			service.RecordAutoModelOutcome(key.group, key.modelName, key.channelID, feedback.success, feedback.latencyMS, feedback.elapsedMS)
		}
	}
}

// maxAutoModelHardSwitches auto 因为"候选服务不了"而换模型的上限（每次请求）。
// 有上限才不会把一个请求放大成几十次上游调用。
const maxAutoModelHardSwitches = 2

// autoModelSwitchableError 判断错误是否属于"换一个候选模型很可能就行"的情况：
// 上游不认这个模型（404）或不认这次请求的某个参数（400/422 等）。
func autoModelSwitchableError(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	switch err.StatusCode {
	case http.StatusNotFound, http.StatusBadRequest, http.StatusUnprocessableEntity,
		http.StatusNotImplemented, http.StatusMethodNotAllowed:
		return true
	}
	return false
}

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError) {
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d): %s", channelError.ChannelId, err.StatusCode, err.Error()))
	// 不要使用context获取渠道信息，异步处理时可能会出现渠道信息不一致的情况
	// do not use context to get channel info, there may be inconsistent channel info when processing asynchronously
	if service.ShouldDisableChannel(err) && channelError.AutoBan {
		gopool.Go(func() {
			service.DisableChannel(channelError, err.ErrorWithStatusCode())
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		// 保存错误日志到mysql中
		userId := c.GetInt("id")
		tokenName := c.GetString("token_name")
		modelName := c.GetString("original_model")
		relayInfo := relaycommon.GetRelayInfo(c)
		if relayInfo != nil && relayInfo.IsModelMappingFullActive() {
			modelName = relayInfo.GetDisplayModelName()
		}
		tokenId := c.GetInt("token_id")
		userGroup := c.GetString("group")
		channelId := c.GetInt("channel_id")
		other := make(map[string]interface{})
		if c.Request != nil && c.Request.URL != nil {
			other["request_path"] = c.Request.URL.Path
		}
		other["error_type"] = err.GetErrorType()
		other["error_code"] = err.GetErrorCode()
		other["status_code"] = err.StatusCode
		other["channel_id"] = channelId
		other["channel_name"] = c.GetString("channel_name")
		other["channel_type"] = c.GetInt("channel_type")
		adminInfo := make(map[string]interface{})
		adminInfo["use_channel"] = c.GetStringSlice("use_channel")
		isMultiKey := common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey)
		if isMultiKey {
			adminInfo["is_multi_key"] = true
			adminInfo["multi_key_index"] = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
		}
		service.AppendChannelAffinityAdminInfo(c, adminInfo)
		other["admin_info"] = adminInfo
		startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
		if startTime.IsZero() {
			startTime = time.Now()
		}
		useTimeSeconds := int(time.Since(startTime).Seconds())
		model.RecordErrorLog(c, userId, channelId, modelName, tokenName, relaycommon.SanitizeModelText(relayInfo, err.MaskSensitiveErrorWithStatusCode()), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
	}

}

func RelayMidjourney(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatMjProxy, nil, nil)

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"description": fmt.Sprintf("failed to generate relay info: %s", err.Error()),
			"type":        "upstream_error",
			"code":        4,
		})
		return
	}

	var mjErr *dto.MidjourneyResponse
	switch relayInfo.RelayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		mjErr = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		mjErr = relay.RelayMidjourneyTask(c, relayInfo.RelayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		mjErr = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		mjErr = relayMidjourneyWithRPMFallback(c, relayInfo, func() *dto.MidjourneyResponse {
			return relay.RelaySwapFace(c, relayInfo)
		})
	default:
		mjErr = relayMidjourneyWithRPMFallback(c, relayInfo, func() *dto.MidjourneyResponse {
			return relay.RelayMidjourneySubmit(c, relayInfo)
		})
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(mjErr)
	if mjErr != nil {
		if mjErr.Description == service.ChannelRPMLimitExceededMessage || mjErr.Description == service.ChannelRPMGroupLimitExceededMessage {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"description": mjErr.Description,
				"type":        "new_api_error",
				"code":        string(types.ErrorCodeChannelRPMLimitExceeded),
			})
			logger.LogInfo(c, fmt.Sprintf("relay RPM limited (channel #%d): %s", c.GetInt("channel_id"), mjErr.Description))
			return
		}
		if mjErr.Description == model.ChannelDailySuccessLimitExceededMessage {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"description": model.ChannelDailySuccessLimitExceededMessage,
				"type":        "new_api_error",
				"code":        string(types.ErrorCodeChannelDailySuccessLimitExceeded),
			})
			logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", c.GetInt("channel_id"), http.StatusTooManyRequests, model.ChannelDailySuccessLimitExceededMessage))
			return
		}
		statusCode := http.StatusBadRequest
		if mjErr.Code == 30 {
			mjErr.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result),
			"type":        "upstream_error",
			"code":        mjErr.Code,
		})
		channelId := c.GetInt("channel_id")
		logger.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", mjErr.Description, mjErr.Result)))
	}
}

func relayMidjourneyWithRPMFallback(c *gin.Context, relayInfo *relaycommon.RelayInfo, attempt func() *dto.MidjourneyResponse) *dto.MidjourneyResponse {
	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}
	for {
		mjErr := attempt()
		if mjErr == nil || mjErr.Description != service.ChannelRPMLimitExceededMessage {
			return mjErr
		}
		if isSpecificChannelRequest(c) || c.GetBool("channel_rpm_locked") {
			return mjErr
		}
		channelID := c.GetInt("channel_id")
		service.MarkChannelRPMLimitSkipped(c, channelID)
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil || channel == nil {
			return service.MidjourneyErrorWrapper(constant.MjRequestError, service.ChannelRPMGroupLimitExceededMessage)
		}
		addUsedChannel(c, channel.Id)
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := types.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := types.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTaskFetch(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}
	if taskErr := relay.RelayTaskFetch(c, relayInfo.RelayMode); taskErr != nil {
		respondTaskError(c, taskErr)
	}
}

func RelayTask(c *gin.Context) {
	relayInfo, err := relaycommon.GenRelayInfo(c, types.RelayFormatTask, nil, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, &dto.TaskError{
			Code:       "gen_relay_info_failed",
			Message:    err.Error(),
			StatusCode: http.StatusInternalServerError,
		})
		return
	}

	if taskErr := relay.ResolveOriginTask(c, relayInfo); taskErr != nil {
		respondTaskError(c, taskErr)
		return
	}

	var result *relay.TaskSubmitResult
	var taskErr *dto.TaskError
	defer func() {
		if taskErr != nil {
			taskErr = service.NormalizeViolationFeeTaskError(relayInfo, taskErr)
			service.RefundBilling(c, relayInfo)
			service.ChargeViolationFeeIfNeeded(c, relayInfo, service.TaskErrorToAPIError(taskErr))
		}
	}()

	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: relayInfo.TokenGroup,
		ModelName:  relayInfo.OriginModelName,
		Retry:      common.GetPointer(0),
	}

	for retryParam.GetRetry() <= retryParam.GetEffectiveRetryTimes() {
		var channel *model.Channel
		lockedChannel := false

		if lockedCh, ok := relayInfo.LockedChannel.(*model.Channel); ok && lockedCh != nil {
			channel = lockedCh
			lockedChannel = true
			if retryParam.GetRetry() > 0 {
				if setupErr := middleware.SetupContextForSelectedChannel(c, channel, relayInfo.OriginModelName); setupErr != nil {
					taskErr = service.TaskErrorWrapperLocal(setupErr.Err, "setup_locked_channel_failed", http.StatusInternalServerError)
					break
				}
			}
		} else {
			var channelErr *types.NewAPIError
			channel, channelErr = getChannel(c, relayInfo, retryParam)
			if channelErr != nil {
				logger.LogError(c, channelErr.Error())
				if service.IsChannelRPMLimitError(channelErr) {
					taskErr = service.TaskErrorFromAPIError(channelErr)
					taskErr.LocalError = true
				} else if isChannelDailySuccessLimitError(channelErr) {
					taskErr = service.TaskErrorWrapperLocal(channelErr.Err, string(types.ErrorCodeChannelDailySuccessLimitExceeded), http.StatusTooManyRequests)
				} else {
					taskErr = service.TaskErrorWrapperLocal(channelErr.Err, "get_channel_failed", http.StatusInternalServerError)
				}
				break
			}
		}

		reservation, reserveErr := reserveChannelDailySuccess(channel)
		if reserveErr != nil {
			if isChannelDailySuccessLimitError(reserveErr) && !lockedChannel && shouldSkipDailyLimitedChannel(c, channel) {
				continue
			}
			taskErr = service.TaskErrorWrapperLocal(reserveErr.Err, string(reserveErr.GetErrorCode()), reserveErr.StatusCode)
			break
		}

		retryParam.SetEffectiveRetryTimesFromChannel(channel)
		addUsedChannel(c, channel.Id)
		bodyStorage, bodyErr := common.GetBodyStorage(c)
		if bodyErr != nil {
			if common.IsRequestBodyTooLargeError(bodyErr) || errors.Is(bodyErr, common.ErrRequestBodyTooLarge) {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusRequestEntityTooLarge)
			} else {
				taskErr = service.TaskErrorWrapperLocal(bodyErr, "read_request_body_failed", http.StatusBadRequest)
			}
			model.ReleaseChannelDailySuccess(reservation)
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		result, taskErr = relay.RelayTaskSubmit(c, relayInfo)
		if taskErr == nil {
			break
		}
		model.ReleaseChannelDailySuccess(reservation)
		apiErr := service.TaskErrorToAPIError(taskErr)
		if service.IsChannelRPMLimitError(apiErr) {
			taskErr.LocalError = true
			if !lockedChannel && shouldSkipRPMLimitedChannel(c, channel) {
				continue
			}
			break
		}
		taskErr = service.NormalizeViolationFeeTaskError(relayInfo, taskErr)

		if !taskErr.LocalError {
			processChannelError(c,
				*types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey,
					common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()),
				service.TaskErrorToAPIError(taskErr))
		}

		if !shouldRetryTaskRelay(c, channel.Id, taskErr, retryParam.GetRemainingRetryTimes()) {
			break
		}
		retryParam.IncreaseRetry()
	}

	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		logger.LogInfo(c, retryLogStr)
	}

	// ── 成功：结算 + 日志 + 插入任务 ──
	if taskErr == nil {
		deferred := result.Quota < 0 ||
			(relayInfo.PriceData.UsePrice && relayInfo.PriceData.ModelPrice < 0) ||
			(!relayInfo.PriceData.UsePrice && relayInfo.PriceData.ModelRatio < 0)
		if !deferred && result.PendingImageRequest == "" {
			if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
				common.SysError("settle task billing error: " + settleErr.Error())
			}
			service.LogTaskConsumption(c, relayInfo)
		}

		task := model.InitTask(result.Platform, relayInfo)
		task.PrivateData.UpstreamTaskID = result.UpstreamTaskID
		task.PrivateData.GMICloudImageRequest = result.PendingImageRequest
		if result.PendingImageRequest != "" {
			task.PrivateData.Key = relayInfo.ApiKey
		}
		task.PrivateData.UpstreamVideoID = relayInfo.UpstreamVideoID
		task.PrivateData.BillingSource = relayInfo.BillingSource
		task.PrivateData.SubscriptionId = relayInfo.SubscriptionId
		task.PrivateData.TokenId = relayInfo.TokenId
		task.PrivateData.BillingContext = &model.TaskBillingContext{
			DeferredSettlement: deferred,
			EstimatedQuota:     result.Quota,
			ModelPrice:         relayInfo.PriceData.ModelPrice,
			GroupRatio:         relayInfo.PriceData.GroupRatioInfo.GroupRatio,
			ModelRatio:         relayInfo.PriceData.ModelRatio,
			OtherRatios:        relayInfo.PriceData.OtherRatios,
			OriginModelName:    relayInfo.OriginModelName,
			PerCallBilling:     common.StringsContains(constant.TaskPricePatches, relayInfo.OriginModelName) || relayInfo.PriceData.UsePrice,
		}
		task.Quota = result.Quota
		if deferred {
			// The signed estimate has not been paid. Only the actual reservation
			// may be returned on failure, including the subscription's minimum.
			task.Quota = relayInfo.FinalPreConsumedQuota
		}
		task.Data = result.TaskData
		task.Action = relayInfo.Action
		if insertErr := task.Insert(); insertErr != nil {
			common.SysError("insert task error: " + insertErr.Error())
			if deferred || result.PendingImageRequest != "" {
				service.RefundBilling(c, relayInfo)
			}
			if isGMICloudImageTask(relayInfo) {
				respondGMICloudImageError(c, http.StatusInternalServerError, "image_task_persist_failed", "Unable to persist image task; no upstream generation was submitted", "")
				return
			}
		} else if isGMICloudImageTask(relayInfo) {
			if !deferred {
				if settleErr := service.SettleBilling(c, relayInfo, result.Quota); settleErr != nil {
					common.SysError("settle image task billing error: " + settleErr.Error())
				}
				service.LogTaskConsumption(c, relayInfo)
			}
			respondGMICloudImageTask(c, task, relayInfo)
		}
	}

	if taskErr != nil {
		taskErr = service.NormalizeViolationFeeTaskError(relayInfo, taskErr)
		respondTaskError(c, taskErr)
	}
}

// respondTaskError 统一输出 Task 错误响应（含 429 限流提示改写）
func respondTaskError(c *gin.Context, taskErr *dto.TaskError) {
	if taskErr.StatusCode == http.StatusTooManyRequests &&
		taskErr.Code != string(types.ErrorCodeChannelDailySuccessLimitExceeded) &&
		taskErr.Code != string(types.ErrorCodeChannelRPMLimitExceeded) {
		taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	if c.Request != nil && c.Request.URL != nil && c.Request.URL.Path == "/v1/images/generations" {
		respondGMICloudImageError(c, taskErr.StatusCode, taskErr.Code, taskErr.Message, "")
		return
	}
	c.JSON(taskErr.StatusCode, taskErr)
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if service.IsViolationFeeTaskError(taskErr) {
		return false
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 5xx 一律重试（含 504/524）。原实现用 IsAlwaysSkipRetryStatusCode 把
		// 504/524 当作“超时不重试”屏蔽掉，但上游损坏时返回的正是这两个码，
		// 屏蔽等于不重试。现改为与同步链路一致：按重试次数换渠道重试完。
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}

// observedAutoModelElapsed measures only this upstream attempt, including failed
// attempts. Request-level StartTime remains reserved for existing logs/billing.
func observedAutoModelElapsed(info *relaycommon.RelayInfo) int64 {
	return info.AttemptElapsed().Milliseconds()
}

func observedAutoLatency(info *relaycommon.RelayInfo, success bool) int64 {
	if info == nil || !success {
		return 0
	}
	if latency, ok := info.AttemptFirstResponseLatency(); ok {
		// Zero is reserved for missing timing. A real sub-millisecond response
		// must still clear an earlier cooldown on a successful attempt.
		return max(int64(1), latency.Milliseconds())
	}
	if info.IsStream {
		// Without a valid first response, generation duration is not a TTFB
		// estimate. Do not turn a fast-starting long stream into a slow outcome.
		return 0
	}
	if info.AttemptStartTime.IsZero() || (!info.AttemptEndTime.IsZero() && info.AttemptEndTime.Before(info.AttemptStartTime)) {
		return 0
	}
	return max(int64(1), observedAutoModelElapsed(info))
}

func observedAutoModelCooldownLatency(info *relaycommon.RelayInfo, success bool) int64 {
	if success {
		return observedAutoLatency(info, true)
	}
	return observedAutoModelElapsed(info)
}
