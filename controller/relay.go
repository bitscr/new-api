package controller

import (
	"encoding/json"
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
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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
			if isAutoModelRequest(c) && relayFormat != types.RelayFormatOpenAIRealtime && c.Writer != nil && c.Writer.Written() {
				return // Never append another response after an auto attempt started delivery.
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

	autoRequest := isAutoModelRequest(c)
	if autoRequest {
		if newAPIError = autoModelCanceledRequestError(c, nil); newAPIError != nil {
			return
		}
		// Middleware plans the request once. Revalidate that exact plan and bind
		// its concrete model before token estimation, pricing or pre-consumption.
		if _, ok := service.GetCurrentAutoModelRoute(c); !ok {
			newAPIError = newAutoModelRouteError("auto model route plan is missing or invalid")
			return
		}
		_, newAPIError = prepareAutoModelRouteFromIndex(c, relayInfo, nil, 0, nil,
			common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex), false)
		if newAPIError != nil {
			return
		}
		if relayInfo.Request != nil {
			request = relayInfo.Request
		}
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

	var autoBudget autoModelAttemptBudget
	for {
		if autoRequest {
			if !autoBudget.canAttempt() || (autoBudget.attempts > 0 && !canRetryAutoModelRequest(c, relayInfo)) {
				break
			}
			retryParam.SetRetry(autoBudget.attempts)
		} else if retryParam.GetRetry() > retryParam.GetEffectiveRetryTimes() {
			break
		}
		if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil {
			newAPIError = canceledErr
			break
		}
		relayInfo.RetryIndex = retryParam.GetRetry()
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			logger.LogError(c, channelErr.Error())
			// The plan is channel-block ordered: exhaust this channel's remaining
			// model routes before trying the next channel, never select at random.
			if trySwitchAutoModel(c, relayInfo, retryParam, tokens, meta) {
				continue
			}
			// Keep the real last failure instead of hiding it behind an exhausted plan.
			if (autoRequest || retryParam.GetRetry() > 0) && relayInfo.LastError != nil {
				newAPIError = relayInfo.LastError
				break
			}
			newAPIError = channelErr
			break
		}

		reservation, reserveErr := reserveChannelDailySuccess(channel)
		if reserveErr != nil {
			newAPIError = reserveErr
			if isChannelDailySuccessLimitError(reserveErr) && shouldSkipDailyLimitedChannel(c, channel) {
				continue
			}
			break
		}

		if !autoRequest {
			retryParam.SetEffectiveRetryTimesFromChannel(channel)
			addUsedChannel(c, channel.Id)
		} else if !relayInfo.PriceData.FreeModel && relayInfo.Billing == nil {
			// A free first model may fall back to a paid model. Establish billing
			// once, before dispatch; an existing session must never be pre-charged again.
			if billingErr := service.PreConsumeBilling(c, relayInfo.PriceData.QuotaToPreConsume, relayInfo); billingErr != nil {
				newAPIError = billingErr
				model.ReleaseChannelDailySuccess(reservation)
				break
			}
		}
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

		if autoRequest {
			if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil {
				newAPIError = canceledErr
				model.ReleaseChannelDailySuccess(reservation)
				break
			}
			if (autoBudget.attempts > 0 && !canRetryAutoModelRequest(c, relayInfo)) || !autoBudget.beginAttempt(channel, retryParam) {
				model.ReleaseChannelDailySuccess(reservation)
				break
			}
			markCurrentAutoModelRouteAttempted(c)
			addUsedChannel(c, channel.Id)
		}
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
			if autoModelClientAbandoned(c, relayInfo) {
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
		if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil && autoModelClientAbandoned(c, relayInfo) {
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

		if autoRequest {
			// Ordinary errors exclude only this (group, model, channel), not every
			// other model on the channel. 400/404 switches spend the same budget
			// as 5xx retries; there is no extra uncounted hard-switch allowance.
			if shouldRetryAutoModelRoute(c, relayInfo, newAPIError, autoBudget.remainingRetries) &&
				trySwitchAutoModel(c, relayInfo, retryParam, tokens, meta) {
				continue
			}
			break
		}
		if !shouldRetry(c, newAPIError, retryParam.GetRemainingRetryTimes()) {
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
	if isAutoModelRequest(c) {
		route, ok := service.GetCurrentAutoModelRoute(c)
		if !ok {
			return nil, newAutoModelRouteError("auto model route plan is missing or invalid")
		}
		// No model-first random selector, used-channel fallback, or fabricated
		// Channel is permitted for auto requests, including the first attempt.
		return getFixedAutoModelChannel(c, info, route)
	}
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

// autoModelAttemptBudget is frozen by the first actual dispatch. Channel/model
// switches cannot reset or enlarge it, including channel-specific retry overrides.
// remainingRetries excludes the first attempt, avoiding an overflowing limit+1.
type autoModelAttemptBudget struct {
	started          bool
	attempts         int
	remainingRetries int
}

func (b *autoModelAttemptBudget) canAttempt() bool {
	return !b.started || b.remainingRetries > 0
}

func (b *autoModelAttemptBudget) beginAttempt(channel *model.Channel, retryParam *service.RetryParam) bool {
	if !b.canAttempt() {
		return false
	}
	if !b.started {
		retryParam.SetEffectiveRetryTimesFromChannel(channel)
		retryParam.SetEffectiveRetryTimes(retryParam.GetEffectiveRetryTimes())
		b.remainingRetries = retryParam.GetEffectiveRetryTimes()
		b.started = true
	} else {
		b.remainingRetries--
	}
	retryParam.SetRetry(b.attempts)
	b.attempts++
	return true
}

func canRetryAutoModelRequest(c *gin.Context, info *relaycommon.RelayInfo) bool {
	return operation_setting.AutoModelEnabled && isAutoModelRequest(c) &&
		autoModelRequestContextError(c) == nil && !c.IsAborted() && !isSpecificChannelRequest(c) &&
		(c.Writer == nil || !c.Writer.Written()) && (info == nil || !info.ClientDeliveryBroken())
}

func shouldRetryAutoModelRoute(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError, remaining int) bool {
	if remaining <= 0 || !canRetryAutoModelRequest(c, info) || err == nil ||
		types.IsSkipRetryError(err) || operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) {
		return false
	}
	return shouldRetry(c, err, remaining) || autoModelSwitchableError(err)
}

const autoModelAttemptedRoutesKey = "auto_model_attempted_routes"

func markCurrentAutoModelRouteAttempted(c *gin.Context) {
	route, ok := service.GetCurrentAutoModelRoute(c)
	if !ok {
		return
	}
	value, _ := c.Get(autoModelAttemptedRoutesKey)
	attempted, _ := value.(map[service.AutoModelRoute]bool)
	if attempted == nil {
		attempted = make(map[service.AutoModelRoute]bool)
	}
	attempted[route] = true
	c.Set(autoModelAttemptedRoutesKey, attempted)
}

func newAutoModelRouteError(message string) *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeGetChannelFailed,
		http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
}

func isAutoModelRouteAuthorized(c *gin.Context, info *relaycommon.RelayInfo, route service.AutoModelRoute) bool {
	if route.ChannelID <= 0 || route.Group == "" || route.ModelName == "" || info == nil ||
		!service.IsAutoModelCandidateAuthorized(c, route.ModelName) || isSpecificChannelRequest(c) {
		return false
	}
	tokenGroup := info.TokenGroup
	if tokenGroup == "" {
		tokenGroup = info.UserGroup
	}
	if tokenGroup != "auto" {
		if route.Group == tokenGroup {
			return true
		}
		// A session-authenticated playground request can have no token group.
		// Preserve its middleware-authorized group, without accepting another
		// group's forged route or broadening the user's usable group set.
		return info.IsPlayground && route.Group == info.UsingGroup &&
			service.GroupInUserUsableGroups(info.UserGroup, route.Group)
	}
	allowed := false
	for _, group := range service.GetRequestGroupCandidates(c, info.UserGroup, tokenGroup) {
		if route.Group == group {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	return common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry) ||
		info.UsingGroup == "" || info.UsingGroup == "auto" || route.Group == info.UsingGroup
}

// getFixedAutoModelChannel is a final authorization/availability boundary. Use the
// enabled-ability snapshot rather than the legacy channel cache, which cannot
// represent disabled abilities, and never substitute another ChannelID.
func getFixedAutoModelChannel(c *gin.Context, info *relaycommon.RelayInfo, route service.AutoModelRoute) (*model.Channel, *types.NewAPIError) {
	if !operation_setting.AutoModelEnabled || !isAutoModelRouteAuthorized(c, info, route) {
		return nil, types.NewErrorWithStatusCode(errors.New("auto model route is not authorized"),
			types.ErrorCodeGetChannelFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	if canceledErr := autoModelCanceledRequestError(c, nil); canceledErr != nil {
		return nil, canceledErr
	}
	if service.GetAutoModelHardExcludedChannelIDs(c)[route.ChannelID] {
		if service.IsChannelRPMLimitSkipped(c, route.ChannelID) {
			return nil, service.NewChannelRPMLimitError(service.ChannelRPMGroupLimitExceededMessage)
		}
		return nil, newChannelDailySuccessLimitError()
	}
	value, _ := c.Get(autoModelAttemptedRoutesKey)
	attempted, _ := value.(map[service.AutoModelRoute]bool)
	if attempted[route] {
		return nil, newAutoModelRouteError("auto model route was already attempted")
	}
	targets, err := model.GetAutoModelRoutingTargets(route.Group)
	if err != nil {
		return nil, newAutoModelRouteError(fmt.Sprintf("cannot revalidate auto model route: %s", err))
	}
	enabled := false
	for _, name := range []string{route.ModelName, ratio_setting.FormatMatchingModelName(route.ModelName)} {
		for _, target := range targets[name] {
			if target.ChannelID == route.ChannelID {
				enabled = true
				break
			}
		}
	}
	if !enabled {
		return nil, newAutoModelRouteError("auto model route is no longer enabled")
	}
	channel, err := model.GetChannelById(route.ChannelID, true)
	if err != nil || channel == nil || channel.Id != route.ChannelID || channel.Status != common.ChannelStatusEnabled {
		return nil, newAutoModelRouteError("auto model route channel is no longer available")
	}
	if service.GetAutoModelCoolingChannelIDs(route.Group, route.ModelName)[route.ChannelID] {
		return nil, newAutoModelRouteError("auto model route is cooling down")
	}
	if channel.Type == constant.ChannelTypeTypeSafe && info.RelayFormat != types.RelayFormatTypeSafe {
		return nil, newAutoModelRouteError("TypeSafe only supports POST /v1/systemone")
	}
	return channel, nil
}

// trySwitchAutoModel advances a route, not a model-only list. Consecutive entries
// may have the same ChannelID or the same ModelName. Neither is grounds to skip.
func trySwitchAutoModel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam, tokens int, meta *types.TokenCountMeta) bool {
	if !canRetryAutoModelRequest(c, info) || info == nil || meta == nil {
		return false
	}
	current, ok := service.GetCurrentAutoModelRoute(c)
	if !ok {
		return false
	}
	channel, err := prepareAutoModelRouteFromIndex(c, info, retryParam, tokens, meta,
		common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex)+1, true)
	if err != nil {
		return false
	}
	logger.LogInfo(c, fmt.Sprintf("auto model: 路线 %s/%s#%d -> %s/%s#%d",
		current.Group, current.ModelName, current.ChannelID, info.UsingGroup, info.OriginModelName, channel.Id))
	return true
}

func prepareAutoModelRouteFromIndex(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam, tokens int, meta *types.TokenCountMeta, index int, reprice bool) (*model.Channel, *types.NewAPIError) {
	routes := service.GetAutoModelRoutePlan(c)
	if index < 0 || index >= len(routes) {
		return nil, newAutoModelRouteError("auto model route plan is exhausted")
	}
	lastErr := newAutoModelRouteError("auto model route plan has no available route")
	for ; index < len(routes); index++ {
		if canceledErr := autoModelCanceledRequestError(c, nil); canceledErr != nil {
			return nil, canceledErr
		}
		if reprice && !canRetryAutoModelRequest(c, info) {
			return nil, lastErr
		}
		channel, routeErr := getFixedAutoModelChannel(c, info, routes[index])
		if routeErr != nil {
			lastErr = routeErr
			continue
		}
		if routeErr = bindAutoModelRoute(c, info, retryParam, routes[index], index, channel, tokens, meta, reprice); routeErr != nil {
			lastErr = routeErr
			continue
		}
		return channel, nil
	}
	return nil, lastErr
}

// Stage all route-dependent state before committing it. A bad price, disabled key
// or malformed body must leave the previous attempt's billing snapshot intact.
func bindAutoModelRoute(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam, route service.AutoModelRoute, index int, channel *model.Channel, tokens int, meta *types.TokenCountMeta, reprice bool) *types.NewAPIError {
	// Keep the original storage owned by the live request even if preparation
	// fails before commit (normally request validation has already cached it).
	if c.Request != nil && c.Request.Body != nil {
		if _, err := common.GetBodyStorage(c); err != nil {
			return types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
		}
	}
	nextCtx := c.Copy()
	if c.Request != nil {
		nextCtx.Request = c.Request.Clone(c.Request.Context())
	}
	if !service.ActivateAutoModelRoute(nextCtx, index) {
		return newAutoModelRouteError("cannot activate auto model route")
	}
	// SetupContext intentionally omits some empty per-channel fields. Clear
	// them first so an Azure/multi-key/organization setting cannot leak across.
	for _, key := range []string{"api_version", "region", "plugin", "bot_id", string(constant.ContextKeyChannelOrganization)} {
		nextCtx.Set(key, "")
	}
	common.SetContextKey(nextCtx, constant.ContextKeyChannelMultiKeyIndex, 0)
	if err := middleware.SetupContextForSelectedChannel(nextCtx, channel, route.ModelName); err != nil {
		return err
	}
	// HandleGroupRatio must not revive a previous auto-group during pricing.
	common.SetContextKey(nextCtx, constant.ContextKeyUsingGroup, route.Group)
	common.SetContextKey(nextCtx, constant.ContextKeyAutoGroup, route.Group)

	nextInfo := *info
	nextInfo.OriginModelName = route.ModelName
	nextInfo.ClientModelName = route.ModelName
	nextInfo.UsingGroup = route.Group
	nextInfo.ModelMappingTargetName = ""
	nextInfo.ModelMappingBypassed = false
	nextInfo.RuntimeHeadersOverride = nil
	nextInfo.UseRuntimeHeadersOverride = false
	nextInfo.ParamOverrideAudit = nil
	nextInfo.ConversationCapture = nil
	nextCtx.Set("conversation_capture", nil)
	nextInfo.RequestConversionChain = nil
	nextInfo.FinalRequestRelayFormat = ""
	nextInfo.InitRequestConversionChain()
	nextInfo.PriceData = types.PriceData{}
	nextInfo.TieredBillingSnapshot = nil
	nextInfo.BillingRequestInput = nil
	// InitChannelMeta normally mutates the shared Request DTO. Delay that one
	// mutation until the staging operation (including pricing) has succeeded.
	nextInfo.Request = nil
	nextInfo.InitChannelMeta(nextCtx)
	nextInfo.Request = info.Request

	oldStorage, newStorage, err := rewriteAutoModelRequestBody(nextCtx, route.ModelName)
	if err != nil {
		return types.NewError(err, types.ErrorCodeReadRequestBodyFailed, types.ErrOptionWithSkipRetry())
	}
	committed := false
	defer func() {
		if !committed && newStorage != nil {
			_ = newStorage.Close()
		}
	}()
	if reprice {
		priceData, err := helper.ModelPriceHelper(nextCtx, &nextInfo, tokens, meta)
		if err != nil {
			return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithSkipRetry())
		}
		nextInfo.PriceData = priceData
	}
	if canceledErr := autoModelCanceledRequestError(c, nil); canceledErr != nil {
		return canceledErr
	}
	if reprice && !canRetryAutoModelRequest(c, info) {
		return newAutoModelRouteError("auto model response already started")
	}
	for key, value := range nextCtx.Keys {
		c.Set(key, value)
	}
	c.Request = nextCtx.Request
	*info = nextInfo
	if info.Request != nil {
		info.Request.SetModelName(route.ModelName)
	}
	relaycommon.SetRelayInfo(c, info)
	if retryParam != nil {
		retryParam.ModelName = route.ModelName
	}
	committed = true
	if newStorage != nil && oldStorage != nil {
		_ = oldStorage.Close()
	}
	return nil
}

// Raw pass-through paths read BodyStorage instead of Request. Replace only the
// JSON model field with RawMessages so unknown fields and explicit zero values
// survive. The caller owns the new storage until the staged route is committed.
func rewriteAutoModelRequestBody(c *gin.Context, modelName string) (common.BodyStorage, common.BodyStorage, error) {
	if c.Request == nil || c.Request.Body == nil {
		return nil, nil, nil
	}
	contentType := strings.TrimSpace(strings.ToLower(strings.SplitN(c.Request.Header.Get("Content-Type"), ";", 2)[0]))
	if contentType != "" && contentType != "application/json" && !strings.HasSuffix(contentType, "+json") {
		return nil, nil, nil // Never parse multipart or form bodies as JSON.
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, nil, err
	}
	body, err := storage.Bytes()
	if err != nil {
		return nil, nil, err
	}
	if len(body) == 0 {
		return storage, nil, nil // Bodyless protocols (e.g. WebSocket upgrade).
	}
	var fields map[string]json.RawMessage
	if err = common.Unmarshal(body, &fields); err != nil {
		return nil, nil, err
	}
	rawModel, ok := fields["model"]
	if !ok {
		return storage, nil, nil // e.g. Gemini models are carried in the URL.
	}
	var previousModel string
	if common.Unmarshal(rawModel, &previousModel) == nil && previousModel == modelName {
		return storage, nil, nil
	}
	fields["model"], err = common.Marshal(modelName)
	if err != nil {
		return nil, nil, err
	}
	body, err = common.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	replacement, err := common.CreateBodyStorage(body)
	if err != nil {
		return nil, nil, err
	}
	c.Set(common.KeyBodyStorage, replacement)
	c.Set(common.KeyRequestBody, nil)
	c.Set(gin.BodyBytesKey, body)
	c.Request.Body = io.NopCloser(replacement)
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Del("Content-Length")
	return storage, replacement, nil
}

// recordAutoModelFeedback captures this attempt now; flushing later must not read
// mutable RelayInfo fields belonging to another model/channel or a later attempt.
func recordAutoModelFeedback(c *gin.Context, info *relaycommon.RelayInfo, success bool) {
	if !isAutoModelRequest(c) || info == nil || autoModelClientAbandoned(c, info) {
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
	if !isAutoModelRequest(c) || info == nil || autoModelClientAbandoned(c, info) {
		return
	}
	queueAutoModelFeedback(c, info, autoModelFeedback{unusable: true, reason: reason})
}

// autoModelClientAbandoned 判断"客户端在拿到本次交付物之前就离开了"。
//
// 不能只看 c.Request.Context().Err()：客户端收完正文后正常关闭连接（curl、SDK、
// 下位机都是收到完整响应立即断开）时 ctx 同样是 canceled，于是每个正常完成的请求
// 都会被判成"客户端断开"，全部 auto 反馈被丢弃——分数不更新、慢成功的冷却不触发。
// 所以以"写入没能完成"为主信号（ClientDeliveryBroken），ctx 取消只在一个字节都
// 没写出去（客户端在等到任何内容前就断了）时才成立。
func autoModelClientAbandoned(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if info != nil && info.ClientDeliveryBroken() {
		return true
	}
	if autoModelRequestContextError(c) == nil {
		return false
	}
	return c.Writer == nil || !c.Writer.Written()
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
