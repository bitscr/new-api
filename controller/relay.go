package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
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
	relayStartedAt := time.Now()
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
			logModelRequestError(c, newAPIError)
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
	// Trial slots are released by flushAutoModelFeedback after that route's
	// observation is published; a slot must not be reusable while its feedback is
	// still only queued and another long backup attempt is running. This safety
	// net runs after flush (LIFO) even if publication itself panics.
	defer releaseAllAutoModelTrialSlots(c)
	defer flushAutoModelFeedback(c)

	var autoBudget autoModelAttemptBudget
	autoRequestStartedAt := relayInfo.StartTime
	if autoRequestStartedAt.IsZero() {
		autoRequestStartedAt = relayStartedAt
	}
	autoStopReason := "completed"
	if autoRequest {
		// Registered before the staging cleanup so it runs after Restore and sees
		// the final wire state.
		defer func() {
			lastCode := ""
			if newAPIError != nil {
				lastCode = string(newAPIError.GetErrorCode())
			}
			logger.LogInfo(c, fmt.Sprintf("auto route diag: event=stop reason=%s attempts=%d max=%d real_dispatched=%d remaining=%d written=%t last_error_code=%s elapsed_ms=%d client_model=%s request_elapsed_ms=%d last_attempt_elapsed_ms=%d %s trace=[%s] advice=%q",
				autoStopReason, autoBudget.attempts, operation_setting.AutoModelMaxAttempts(),
				autoBudget.dispatched, autoBudget.remainingRetries(),
				c.Writer != nil && c.Writer.Written(), lastCode,
				relayInfo.AttemptElapsed().Milliseconds(), autoModelTraceText(relayInfo.ClientModelName, 100),
				time.Since(autoRequestStartedAt).Milliseconds(), relayInfo.AttemptElapsed().Milliseconds(), autoModelRequestDeadlineLog(c),
				formatAutoModelAttemptTrace(c), autoModelRetryAdvice(autoStopReason, newAPIError)))
		}()
	}
	// Stage one auto candidate until the answer observer confirms real output.
	// The cleanup defer is registered after feedback flush, so it runs first and
	// restores c.Writer before any fallback error/feedback path can write.
	var stagedAutoResponse *relaycommon.AutoAttemptResponseWriter
	defer func() {
		if stagedAutoResponse != nil {
			stagedAutoResponse.Discard()
			stagedAutoResponse.Restore()
			stagedAutoResponse = nil
		}
	}()
	for {
		if stagedAutoResponse != nil {
			stagedAutoResponse.Discard()
			stagedAutoResponse.Restore()
			stagedAutoResponse = nil
		}
		if autoRequest {
			if !autoBudget.canAttempt() {
				autoStopReason = "budget_exhausted"
				break
			}
			if autoBudget.attempts > 0 && !canRetryAutoModelRequest(c, relayInfo) {
				autoStopReason = "request_not_retryable"
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=%s attempt=%d max=%d remaining=%d",
					autoModelRetryBlockedReason(c, relayInfo), autoBudget.attempts,
					operation_setting.AutoModelMaxAttempts(), autoBudget.remainingRetries()))
				break
			}
			retryParam.SetRetry(autoBudget.attempts)
		} else if retryParam.GetRetry() > retryParam.GetEffectiveRetryTimes() {
			break
		}
		if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil {
			autoStopReason = "client_canceled"
			newAPIError = canceledErr
			break
		}
		relayInfo.RetryIndex = retryParam.GetRetry()
		channel, channelErr := getChannel(c, relayInfo, retryParam)
		if channelErr != nil {
			// auto 的本地路线拒绝（冷却、已试过、资格失效等）是正常跳过，
			// 后续还会试别的路线；只有最终失败才由外层按 ERR 记录。
			if isAutoModelRequest(c) && types.IsSkipRetryError(channelErr) {
				logger.LogInfo(c, channelErr.Error())
			} else {
				logger.LogError(c, channelErr.Error())
			}
			// 派发前就拿不到渠道:当前路线同样进本地跳过集合,重排不会把它放回队首。
			if current, ok := service.GetCurrentAutoModelRoute(c); ok {
				if isAutoModelRequest(c) {
					logger.LogInfo(c, fmt.Sprintf("auto route diag: event=local_reject group=%s model=%s channel=%d attempt=%d max=%d",
						current.Group, current.ModelName, current.ChannelID, autoBudget.attempts,
						operation_setting.AutoModelMaxAttempts()))
				}
				markAutoModelRouteLocallyRejected(c, current)
			}
			// auto 计划按"渠道 priority 降序 → 可靠性准入 → 成功延迟池 → 池内两级加权随机"排好;
			// 换路 = 对剩余候选重新规划,既不是逐个试完本渠道的剩余模型,也不是随机选渠。
			if trySwitchAutoModelWithReason(c, relayInfo, retryParam, tokens, meta, "local_route_rejected", autoBudget) {
				continue
			}
			// Keep the real last failure instead of hiding it behind an exhausted plan.
			if (autoRequest || retryParam.GetRetry() > 0) && relayInfo.LastError != nil {
				newAPIError = relayInfo.LastError
				autoStopReason = "no_available_route"
				break
			}
			newAPIError = channelErr
			autoStopReason = "no_available_route"
			break
		}

		reservation, reserveErr := reserveChannelDailySuccess(channel)
		if reserveErr != nil {
			newAPIError = reserveErr
			if isChannelDailySuccessLimitError(reserveErr) && shouldSkipDailyLimitedChannel(c, channel) {
				continue
			}
			autoStopReason = "channel_reservation_failed"
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
				autoStopReason = "billing_failed"
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
			autoStopReason = "body_read_failed"
			break
		}
		c.Request.Body = io.NopCloser(bodyStorage)

		if autoRequest {
			if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil {
				newAPIError = canceledErr
				model.ReleaseChannelDailySuccess(reservation)
				autoStopReason = "client_canceled"
				break
			}
			if currentRoute, ok := service.GetCurrentAutoModelRoute(c); ok && service.IsAutoModelTrialRoute(c, currentRoute) {
				release, acquired := service.AcquireAutoModelTrialSlot(currentRoute)
				if !acquired {
					model.ReleaseChannelDailySuccess(reservation)
					markAutoModelRouteLocallyRejected(c, currentRoute)
					newAPIError = newAutoModelRouteError("auto model trial route is busy")
					if trySwitchAutoModelWithReason(c, relayInfo, retryParam, tokens, meta, "trial_busy", autoBudget) {
						autoStopReason = "trial_busy_switched"
						continue
					}
					autoStopReason = "trial_busy"
					break
				}
				holdAutoModelTrialSlot(c, currentRoute, release)
			}
			if (autoBudget.attempts > 0 && !canRetryAutoModelRequest(c, relayInfo)) || !autoBudget.reserveAttempt() {
				model.ReleaseChannelDailySuccess(reservation)
				if currentRoute, ok := service.GetCurrentAutoModelRoute(c); ok {
					releaseAutoModelTrialSlot(c, currentRoute)
				}
				autoStopReason = "budget_or_request_guard"
				break
			}
			markCurrentAutoModelRouteAttempted(c)
			addUsedChannel(c, channel.Id)
			logger.LogInfo(c, fmt.Sprintf("auto route diag: event=attempt stage=selected attempt=%d max=%d prior_dispatched=%d remaining_if_dispatched=%d group=%s model=%s channel=%d client_model=%s route={%s}",
				autoBudget.attempts, operation_setting.AutoModelMaxAttempts(), autoBudget.dispatched,
				max(0, operation_setting.AutoModelMaxAttempts()-autoBudget.dispatched-1),
				relayInfo.UsingGroup, relayInfo.OriginModelName, channel.Id, relayInfo.ClientModelName,
				service.AutoModelRouteDiagnostic(service.GetAutoModelRoutePlanMeta(c), service.AutoModelRoute{Group: relayInfo.UsingGroup, ModelName: relayInfo.OriginModelName, ChannelID: channel.Id})))
		}
		relayInfo.BeginAttempt()
		if autoRequest && relayFormat != types.RelayFormatOpenAIRealtime && relayFormat != types.RelayFormatMistralRealtime {
			stagedAutoResponse = relaycommon.InstallAutoAttemptResponseWriter(c)
		}
		var explorationParentRequest *http.Request
		if stagedAutoResponse != nil && autoBudget.remainingRetries() > 1 && canRetryAutoModelRequest(c, relayInfo) {
			if timeout := service.AutoModelExplorationAttemptTimeout(c); timeout > 0 {
				explorationParentRequest = c.Request
				attemptContext, guard := relaycommon.NewAutoExplorationAttempt(c.Request.Context(), timeout)
				relayInfo.AutoExplorationAttempt = guard
				stagedAutoResponse.SetExplorationAttempt(guard)
				c.Request = c.Request.WithContext(attemptContext)
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=exploration_guard group=%s model=%s channel=%d wait_ms=%d fallback=%s %s",
					relayInfo.UsingGroup, relayInfo.OriginModelName, channel.Id, timeout.Milliseconds(), autoModelImmediateFallbackLog(c), autoModelRequestDeadlineLog(c)))
			}
		}
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
		if explorationParentRequest != nil {
			timedOut := relayInfo.AutoExplorationAttempt.Finish()
			relayInfo.AutoExplorationAttempt = nil
			c.Request = explorationParentRequest
			if timedOut && !stagedAutoResponse.Committed() {
				newAPIError = types.NewErrorWithStatusCode(relaycommon.ErrAutoExplorationTimeout,
					types.ErrorCodeChannelResponseTimeExceeded, http.StatusGatewayTimeout)
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=exploration_timeout group=%s model=%s channel=%d action=retry_pair wait_ms=%d elapsed_ms=%d dispatch_elapsed_ms=%d fallback=%s %s advice=%q",
					relayInfo.UsingGroup, relayInfo.OriginModelName, channel.Id, service.AutoModelExplorationAttemptTimeout(c).Milliseconds(),
					relayInfo.AttemptElapsed().Milliseconds(), relayInfo.AttemptDispatchElapsed().Milliseconds(), autoModelImmediateFallbackLog(c), autoModelRequestDeadlineLog(c),
					"若连续出现未验证探索超时，可降低AutoModelExploreRatio和AutoModelExploreMaxRatio；手动优先级和成功慢路线不受该等待上限影响"))
			}
		}
		relayInfo.EndAttempt()
		if autoRequest {
			// 只有真正进入上游派发的尝试才占用 auto 的真实派发预算。
			dispatched := relayInfo.HasUpstreamDispatch()
			autoBudget.confirmAttempt(dispatched)
			if dispatched {
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=attempt stage=dispatched attempt=%d max=%d real_dispatched=%d remaining=%d group=%s model=%s channel=%d client_model=%s",
					autoBudget.attempts, operation_setting.AutoModelMaxAttempts(), autoBudget.dispatched,
					autoBudget.remainingRetries(), relayInfo.UsingGroup, relayInfo.OriginModelName,
					channel.Id, relayInfo.ClientModelName))
			}
			if !dispatched {
				// 本地拒绝/转换失败没有上游证据，试用名额立即可用；已派发的尝试
				// 则保留名额，直到 flush 发布它的反馈。
				if currentRoute, ok := service.GetCurrentAutoModelRoute(c); ok {
					releaseAutoModelTrialSlot(c, currentRoute)
				}
			}
		}

		if newAPIError == nil {
			if autoModelClientAbandoned(c, relayInfo) {
				// Some stream adaptors return nil on client disconnect. Do not
				// classify a partial answer as healthy or as an upstream failure.
				traceAutoModelAttempt(c, relayInfo, "client_abandoned", "")
				autoStopReason = "client_abandoned"
				relayInfo.LastError = nil
				return
			}
			// 200 但没有有效回答（正文为空，或正文只是上游网关的告警横幅）不算成功。
			// auto 请求在响应尚未提交时可以先丢弃这次候选的暂存响应，再按同一请求的
			// 重排结果切换；具名请求保持原来的 200 交付行为。
			unusable, reason := relayInfo.ClientAnswerUnusable()
			if !unusable && isAutoModelRequest(c) && !relayInfo.ClientAnswerHasValidOutput() {
				// A handler that returns 200 without writing a single body byte is
				// the non-stream empty-response case; the observation layer has no
				// payload to classify in that situation. Realtime formats deliver
				// over the websocket and never touch this writer.
				stagedEmpty := stagedAutoResponse != nil && !stagedAutoResponse.Committed() &&
					stagedAutoResponse.StagedBytes() == 0
				unstagedEmpty := stagedAutoResponse == nil &&
					relayFormat != types.RelayFormatOpenAIRealtime && relayFormat != types.RelayFormatMistralRealtime &&
					(c.Writer == nil || !c.Writer.Written())
				if stagedEmpty || unstagedEmpty {
					unusable = true
					reason = "200 但响应正文为空（无正文/思考内容/工具调用）"
				}
			}
			if unusable {
				model.ReleaseChannelDailySuccess(reservation)
				failureClass := autoModelUnusableFailureClass(reason)
				if isAutoModelRequest(c) {
					logger.LogInfo(c, fmt.Sprintf("auto model: 候选 %s 没有给出有效回答（%s），不计成功", relayInfo.OriginModelName, failureClass))
				} else {
					logger.LogError(c, fmt.Sprintf("渠道 #%d 的 %s 没有给出有效回答（%s），HTTP 200 但响应为空", channel.Id, relayInfo.OriginModelName, failureClass))
				}
				if isAutoModelRequest(c) {
					traceAutoModelAttempt(c, relayInfo, "invalid_answer", failureClass)
					recordAutoModelUnusableAnswer(c, relayInfo, failureClass)
				} else {
					recordAutoModelUnusableAnswer(c, relayInfo, reason)
				}
				if !isAutoModelRequest(c) {
					// 非 auto 保持现有行为：先把原 200 交付给客户端，再按渠道级错误处理。
					if stagedAutoResponse != nil {
						if err := stagedAutoResponse.Commit(); err != nil {
							relayInfo.MarkClientDeliveryBroken()
						}
					}
					unusableErr := types.NewErrorWithStatusCode(errors.New(reason), types.ErrorCodeBadResponse, http.StatusOK, types.ErrOptionWithSkipRetry())
					processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), unusableErr)
					autoStopReason = "invalid_answer_named_model"
					relayInfo.LastError = nil
					return
				}
				protocolErr := relayInfo.UpstreamProtocolError()
				if protocolErr != nil {
					recordRelayErrorLog(c, protocolErr)
				} else {
					recordRelayErrorLog(c, types.NewErrorWithStatusCode(errors.New("HTTP 200 unusable answer: "+failureClass), types.ErrorCodeBadResponse, http.StatusOK, types.ErrOptionWithSkipRetry()))
				}
				blocked := autoModelInvalidSwitchBlockedReason(c, relayInfo, autoBudget, stagedAutoResponse)
				if blocked == "" && stagedAutoResponse != nil {
					stagedAutoResponse.Discard()
				}
				// Publish the completed failure even when no switch is possible:
				// the next request in this process must see the route cooling now.
				flushAutoModelFeedback(c)
				if blocked == "" {
					if trySwitchAutoModelWithReason(c, relayInfo, retryParam, tokens, meta, "invalid_answer_"+failureClass, autoBudget) {
						autoStopReason = "invalid_answer_switched"
						relayInfo.LastError = nil
						continue
					}
					blocked = "no_next_route"
				}
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=%s failure=%s attempt=%d max=%d remaining=%d written=%t",
					blocked, failureClass, autoBudget.attempts, operation_setting.AutoModelMaxAttempts(),
					autoBudget.remainingRetries(), c.Writer != nil && c.Writer.Written()))
				autoStopReason = "invalid_answer_unretryable"
				if !autoModelResponseDelivered(c, stagedAutoResponse) {
					newAPIError = newAutoModelUnusableAnswerError()
					break
				}
				relayInfo.LastError = nil
				return
			}
			if relayInfo.StreamStatus != nil && relayInfo.StreamStatus.IsUpstreamFailure() {
				// 上游流异常结束不是成功。已经交付过有效内容时只记录失败；尚未提交
				// 任何内容时，仍可在预算内安全换路。
				reason := relayInfo.StreamStatus.Summary()
				model.ReleaseChannelDailySuccess(reservation)
				recordAutoModelStreamFailure(c, relayInfo, reason)
				streamErr := types.NewErrorWithStatusCode(errors.New("upstream stream ended abnormally: "+reason),
					types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
				if isAutoModelRequest(c) {
					traceAutoModelAttempt(c, relayInfo, "stream_failed", reason)
					logger.LogInfo(c, fmt.Sprintf("auto model: 候选 %s 的流异常结束，不计成功", relayInfo.OriginModelName))
					recordRelayErrorLog(c, streamErr)
				} else {
					logger.LogError(c, fmt.Sprintf("渠道 #%d 的 %s 流异常结束（%s）", channel.Id, relayInfo.OriginModelName, reason))
					if stagedAutoResponse != nil {
						if err := stagedAutoResponse.Commit(); err != nil {
							relayInfo.MarkClientDeliveryBroken()
						}
					}
					processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), streamErr)
					autoStopReason = "stream_failed_named_model"
					relayInfo.LastError = nil
					return
				}
				blocked := autoModelInvalidSwitchBlockedReason(c, relayInfo, autoBudget, stagedAutoResponse)
				if blocked == "" && stagedAutoResponse != nil {
					stagedAutoResponse.Discard()
				}
				// A completed stream failure is published before any switch decision,
				// so an exhausted budget does not delay this process's cooldown.
				flushAutoModelFeedback(c)
				if blocked == "" {
					if trySwitchAutoModelWithReason(c, relayInfo, retryParam, tokens, meta, "stream_failed", autoBudget) {
						autoStopReason = "stream_failed_switched"
						relayInfo.LastError = nil
						continue
					}
					blocked = "no_next_route"
				}
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=%s failure=stream_interrupted attempt=%d max=%d remaining=%d written=%t",
					blocked, autoBudget.attempts, operation_setting.AutoModelMaxAttempts(),
					autoBudget.remainingRetries(), c.Writer != nil && c.Writer.Written()))
				autoStopReason = "stream_failed_unretryable"
				if !autoModelResponseDelivered(c, stagedAutoResponse) {
					newAPIError = streamErr
					break
				}
				relayInfo.LastError = nil
				return
			}
			if stagedAutoResponse != nil {
				if err := stagedAutoResponse.Commit(); err != nil {
					relayInfo.MarkClientDeliveryBroken()
					traceAutoModelAttempt(c, relayInfo, "commit_failed", err.Error())
					autoStopReason = "response_commit_failed"
					relayInfo.LastError = nil
					return
				}
			}
			if autoModelAnswerOutcomeUnknown(c, relayInfo, stagedAutoResponse) {
				// The response was delivered, but the observer could not judge it
				// (unknown shape, truncated/ambiguous choices or a staging size cap).
				// Neutral evidence: neither success nor failure may be recorded.
				traceAutoModelAttempt(c, relayInfo, "unknown_answer", "")
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=unknown_answer attempt=%d max=%d remaining=%d overflow=%t",
					autoBudget.attempts, operation_setting.AutoModelMaxAttempts(), autoBudget.remainingRetries(),
					stagedAutoResponse.Overflow()))
				autoStopReason = "answer_undetermined"
				relayInfo.LastError = nil
				return
			}
			traceAutoModelAttempt(c, relayInfo, "ok", "")
			recordAutoModelFeedback(c, relayInfo, true)
			autoStopReason = "success"
			relayInfo.LastError = nil
			return
		}
		model.ReleaseChannelDailySuccess(reservation)
		if canceledErr := autoModelCanceledRequestError(c, newAPIError); canceledErr != nil && autoModelClientAbandoned(c, relayInfo) {
			// The client disappearing is not evidence against this upstream. Keep
			// earlier real failures queued, but do not penalize or retry this attempt.
			// Elapsed time does not change that rule: a proven slow successful
			// model can legitimately outlast the client's wait. Unverified trial
			// timeouts are owned by the gateway's separate exploration guard.
			traceAutoModelAttempt(c, relayInfo, "client_gone", "")
			autoStopReason = "client_canceled"
			newAPIError = canceledErr
			break
		}
		if service.IsChannelRPMLimitError(newAPIError) {
			traceAutoModelAttempt(c, relayInfo, "rpm_limit", string(newAPIError.GetErrorCode()))
			relayInfo.LastError = newAPIError
			if shouldSkipRPMLimitedChannel(c, channel) {
				continue
			}
			break
		}

		newAPIError = service.NormalizeViolationFeeErrorForRelay(relayInfo, newAPIError)
		relayInfo.LastError = newAPIError

		if errors.Is(newAPIError, relaycommon.ErrAutoExplorationTimeout) {
			// The gateway ended one speculative model attempt; this is not evidence
			// to disable its channel and lose the working fallback on that channel.
			recordRelayErrorLog(c, newAPIError)
		} else {
			processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, channel.ChannelInfo.IsMultiKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey), channel.GetAutoBan()), newAPIError)
		}
		// 所有失败照常记录并按判据分档——包括请求体错误(400/422 且文本指向请求体):
		// 它们对"这个组合能不能服务好本次调用方"同样是证据,照常进普通冷却。
		// 上下文超长等保护项仍在 AutoModelPermanentFailure 内部优先处理;命中确定性
		// 关键词的组合会被直接摘除,其余失败走普通冷却阶梯。
		// 早先请求体错误有豁免(不记冷却),已按使用者决策移除:工具负载被拒也是证据,
		// 豁免会让这类组合永远留在候选池里反复被选中。
		recordAutoModelUpstreamFailure(c, relayInfo, newAPIError)
		traceAutoModelAttempt(c, relayInfo, "error",
			fmt.Sprintf("code=%s status=%d", newAPIError.GetErrorCode(), newAPIError.StatusCode))

		if autoRequest {
			// Model-specific failures retain sibling-model fallback. Channel-scoped
			// failures skip its remaining models for this request only. Both spend
			// the same frozen budget and retain pair-only persistent feedback.
			retryable := prepareAutoModelRetry(c, relayInfo, newAPIError, autoBudget.remainingRetries())
			// Publish this completed failure before the replacement plan is built
			// and regardless of whether another route exists: the next request must
			// see this route cooling immediately.
			flushAutoModelFeedback(c)
			if retryable {
				if trySwitchAutoModelWithReason(c, relayInfo, retryParam, tokens, meta, "upstream_error_"+string(newAPIError.GetErrorCode()), autoBudget) {
					autoStopReason = "upstream_error_switched"
					continue
				}
				autoStopReason = "no_next_route"
			} else {
				autoStopReason = "upstream_error_unretryable"
				blocked := autoModelRetryBlockedReason(c, relayInfo)
				if blocked == "" {
					blocked = autoModelRetryIneligibilityReason(c, relayInfo, newAPIError, autoBudget.remainingRetries())
				}
				logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=%s failure=upstream_error code=%s status=%d attempt=%d max=%d remaining=%d advice=%q",
					blocked, newAPIError.GetErrorCode(), newAPIError.StatusCode, autoBudget.attempts,
					operation_setting.AutoModelMaxAttempts(), autoBudget.remainingRetries(), autoModelRetryAdvice(blocked, newAPIError)))
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

// autoModelAttemptBudget 只约束**真实进入上游请求回调**的次数(含首次)。连接失败与
// 上游 429 都是真实派发,必须计数;本地 RPM/每日限额拒绝、派发前资格失效与取消
// 不占真实派发预算,因此也不能借"不占预算"任意重试——本地遍历、请求取消与路线
// 去重仍由独立且有限的约束维持。
//
// 两阶段记账:reserveAttempt 暂占一格 → relay 返回后按 HasUpstreamDispatch 确认或
// 撤销。预算只在第一次真实派发确认时冻结;渠道 override 与换模型/组不能放大它。
// remainingRetries 不含下一次派发,避免出现"上限+1"的溢出。
type autoModelAttemptBudget struct {
	started    bool // 是否已有一次真实派发(预算已冻结)
	attempts   int  // 本次请求已试过的总次数(含本地拒绝),仅用于日志/重试序号
	dispatched int  // 真实上游派发次数,含首次
	reserved   bool // 当前是否已暂占一格,relay 返回后必须 confirm 或撤销
}

func (b *autoModelAttemptBudget) canAttempt() bool {
	return b.dispatched < operation_setting.AutoModelMaxAttempts()
}

func (b *autoModelAttemptBudget) reserveAttempt() bool {
	if !b.canAttempt() || b.reserved {
		return false
	}
	b.reserved = true
	b.attempts++
	return true
}

// confirmAttempt 在 relay 返回后立即调用;只有 dispatches=true 才真正消耗一格预算。
func (b *autoModelAttemptBudget) confirmAttempt(dispatched bool) {
	b.reserved = false
	if !dispatched {
		return
	}
	b.started = true
	b.dispatched++
}

// remainingRetries 是当前剩余的真实重试次数(下一次派发不计入)。
func (b *autoModelAttemptBudget) remainingRetries() int {
	return max(0, operation_setting.AutoModelMaxAttempts()-b.dispatched)
}

func canRetryAutoModelRequest(c *gin.Context, info *relaycommon.RelayInfo) bool {
	return operation_setting.AutoModelEnabled && isAutoModelRequest(c) &&
		autoModelRequestContextError(c) == nil && !c.IsAborted() && !isSpecificChannelRequest(c) &&
		(c.Writer == nil || !c.Writer.Written()) && (info == nil || !info.ClientDeliveryBroken())
}

// prepareAutoModelRetry applies request-scoped model/channel exclusions. It must
// run after every retry guard, before the existing route-plan advancement.
func prepareAutoModelRetry(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError, remaining int) bool {
	if remaining <= 0 || !canRetryAutoModelRequest(c, info) || err == nil ||
		types.IsSkipRetryError(err) || operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) ||
		service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return false
	}
	if info == nil || (!info.HasUpstreamDispatch() && !types.IsChannelError(err)) {
		return false // Local request rejection is not evidence to retry an upstream.
	}
	scope := service.ClassifyAutoModelRetry(err)
	if scope != service.AutoModelRetryStop && errors.Is(err, relaycommon.ErrAutoExplorationTimeout) {
		// A speculative model did not answer in time. The immediate working
		// fallback may share its channel; exclude only the attempted tuple.
		return true
	}
	switch scope {
	case service.AutoModelRetryNextModel:
		// A model-scoped failure means this request must move to a different
		// model, not merely a different channel serving the same model. Keep
		// the exclusion request-local; persistent cooldowns remain tuple-scoped.
		if info.UsingGroup != "" && info.OriginModelName != "" {
			service.MarkAutoModelModelFailed(c, info.UsingGroup, info.OriginModelName)
		}
		return true
	case service.AutoModelRetryNextChannel:
		if info.ChannelMeta == nil || info.ChannelId <= 0 {
			return false // Do not silently turn an unidentifiable channel into a model retry.
		}
		service.MarkAutoModelChannelFailed(c, info.ChannelId)
		return true
	default:
		return false
	}
}

// This describes the existing retry guards for logs only. It never grants an
// extra retry or changes the model/channel classification.
func autoModelRetryIneligibilityReason(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError, remaining int) string {
	if remaining <= 0 {
		return "budget_exhausted"
	}
	if reason := autoModelRetryBlockedReason(c, info); reason != "" {
		return reason
	}
	if err == nil {
		return "no_error"
	}
	if types.IsSkipRetryError(err) {
		return "error_skip_retry"
	}
	if operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) {
		return "error_code_skip_retry"
	}
	if service.ShouldSkipRetryAfterChannelAffinityFailure(c) {
		return "affinity_skip_retry"
	}
	if info == nil || (!info.HasUpstreamDispatch() && !types.IsChannelError(err)) {
		return "not_dispatched"
	}
	if err.StatusCode < 100 || err.StatusCode > 599 {
		return "status_invalid"
	}
	if err.StatusCode >= 200 && err.StatusCode < 300 {
		return "status_success"
	}
	if operation_setting.IsAlwaysSkipRetryStatusCode(err.StatusCode) {
		return "status_skip_retry"
	}
	if err.GetErrorType() == types.ErrorTypeNewAPIError && !types.IsChannelError(err) {
		switch err.GetErrorCode() {
		case types.ErrorCodeInvalidRequest, types.ErrorCodeConvertRequestFailed,
			types.ErrorCodeReadRequestBodyFailed, types.ErrorCodeBadRequestBody:
			return "local_request_error"
		}
	}
	scope := service.ClassifyAutoModelRetry(err)
	if scope == service.AutoModelRetryNextChannel && (info.ChannelMeta == nil || info.ChannelId <= 0) {
		return "channel_unidentified"
	}
	if scope == service.AutoModelRetryStop && !operation_setting.ShouldRetryByStatusCode(err.StatusCode) {
		return "status_not_retryable"
	}
	return "retry_classification"
}

func autoModelRetryAdvice(reason string, err *types.NewAPIError) string {
	switch reason {
	case "budget_exhausted":
		return "真实派发预算已耗尽；若仍有成功后备，可适度增加AutoModelMaxAttempts，同时检查前序失败路线"
	case "status_not_retryable":
		if err != nil {
			return fmt.Sprintf("HTTP %d未获当前重试规则允许；确认该上游错误可换路后检查AutomaticRetryStatusCodes，勿为本地请求错误盲目增加预算", err.StatusCode)
		}
	case "client_canceled", "client_abandoned":
		return "客户端已取消；结合request_elapsed_ms与历史成功延迟检查客户端等待时间，增加网关预算不能恢复已取消请求"
	case "error_skip_retry", "error_code_skip_retry", "affinity_skip_retry":
		return "明确的禁重试规则生效；增加AutoModelMaxAttempts不会改变该限制"
	case "not_dispatched":
		return "未派发上游，检查本地请求转换或渠道资格；无需通过冷却参数处理"
	case "local_request_error":
		return "网关本地请求错误，检查请求体与转换；修改AutomaticRetryStatusCodes或增加预算不能绕过该保护"
	case "status_skip_retry":
		return "HTTP状态命中固定禁重试保护；修改AutomaticRetryStatusCodes或增加预算不会改变该限制"
	case "status_invalid", "status_success":
		return "错误对象的HTTP状态异常或为成功状态；检查上游适配及错误封装，不能用重试状态码配置绕过"
	case "channel_unidentified":
		return "换渠道失败缺少有效渠道身份；检查渠道元数据，增加预算不能修复该问题"
	}
	return ""
}

func autoModelRequestDeadlineLog(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return "client_deadline_remaining_ms=?"
	}
	if deadline, ok := c.Request.Context().Deadline(); ok {
		return fmt.Sprintf("client_deadline_remaining_ms=%d", max(int64(0), time.Until(deadline).Milliseconds()))
	}
	return "client_deadline_remaining_ms=?"
}

func autoModelImmediateFallbackLog(c *gin.Context) string {
	current, ok := service.GetCurrentAutoModelRoute(c)
	if !ok {
		return "-"
	}
	routes := service.GetAutoModelRoutePlan(c)
	for i, route := range routes {
		if route == current && i+1 < len(routes) {
			fallback := routes[i+1]
			return fmt.Sprintf("%s/%s#%d", autoModelTraceText(fallback.Group, 80), autoModelTraceText(fallback.ModelName, 100), fallback.ChannelID)
		}
	}
	return "-"
}

const autoModelAttemptedRoutesKey = "auto_model_attempted_routes"
const autoModelLocallyRejectedRoutesKey = "auto_model_locally_rejected_routes"

// autoModelMaxLocalRejections 是单次请求允许的"派发前本地拒绝"总数上限。
//
// 本地拒绝(RPM 已满、每日限额、资格复验失败、渠道被排除)按设计不占真实派发
// 预算,所以它必须有自己独立且有限的上界:否则一份在请求进行中被不断扩大的
// 候选配置,能让单个请求无限遍历下去。跳过集合保证每条三元组最多被评估一次,
// 这个上限再给"配置持续增长"这种情况一个硬边界。
const autoModelMaxLocalRejections = 64

// attemptedAutoModelRoutes 返回本次请求已经打过的路由集合。返回的是活跃的 map,
// 调用方只读;要写入请走 markCurrentAutoModelRouteAttempted。
func attemptedAutoModelRoutes(c *gin.Context) map[service.AutoModelRoute]bool {
	if c == nil {
		return nil
	}
	value, _ := c.Get(autoModelAttemptedRoutesKey)
	attempted, _ := value.(map[service.AutoModelRoute]bool)
	return attempted
}

func markCurrentAutoModelRouteAttempted(c *gin.Context) {
	route, ok := service.GetCurrentAutoModelRoute(c)
	if !ok {
		return
	}
	attempted := attemptedAutoModelRoutes(c)
	if attempted == nil {
		attempted = make(map[service.AutoModelRoute]bool)
	}
	attempted[route] = true
	c.Set(autoModelAttemptedRoutesKey, attempted)
}

// locallyRejectedAutoModelRoutes 返回本次请求在**派发前**就被本地规则拒绝的路线。
//
// 它与 attempted 分开记录:attempted 是"真的打过这条三元组",这里是"还没派发
// 就被否掉"。两者都必须从重排结果里排除,否则重排会把同一条失效路线反复放回
// 队首,请求在"重排 → 队首被拒 → 再重排"之间空转。返回的是活跃 map,调用方只读。
func locallyRejectedAutoModelRoutes(c *gin.Context) map[service.AutoModelRoute]bool {
	if c == nil {
		return nil
	}
	value, _ := c.Get(autoModelLocallyRejectedRoutesKey)
	rejected, _ := value.(map[service.AutoModelRoute]bool)
	return rejected
}

// markAutoModelRouteLocallyRejected 把一条派发前被拒的路线记进本请求的跳过集合。
// 返回 false 表示本请求的本地拒绝已经到达上限,调用方必须停止继续遍历。
func markAutoModelRouteLocallyRejected(c *gin.Context, route service.AutoModelRoute) bool {
	if c == nil || route.ChannelID <= 0 || route.Group == "" || route.ModelName == "" {
		return true
	}
	rejected := locallyRejectedAutoModelRoutes(c)
	if rejected == nil {
		rejected = make(map[service.AutoModelRoute]bool)
	}
	rejected[route] = true
	c.Set(autoModelLocallyRejectedRoutesKey, rejected)
	if len(rejected) >= autoModelMaxLocalRejections {
		return false
	}
	return true
}

func autoModelRetryBlockedReason(c *gin.Context, info *relaycommon.RelayInfo) string {
	if !operation_setting.AutoModelEnabled {
		return "auto_disabled"
	}
	if !isAutoModelRequest(c) {
		return "not_auto_request"
	}
	if autoModelRequestContextError(c) != nil {
		return "client_canceled"
	}
	if c.IsAborted() {
		return "request_aborted"
	}
	if isSpecificChannelRequest(c) {
		return "specific_channel_locked"
	}
	if c.Writer != nil && c.Writer.Written() {
		return "response_committed"
	}
	if info != nil && info.ClientDeliveryBroken() {
		return "delivery_broken"
	}
	return ""
}

// autoModelResponseDelivered reports whether this candidate already put a
// response on the wire. A failure after that point must never be replaced by a
// second HTTP response, and must never end silently without one.
func autoModelResponseDelivered(c *gin.Context, staged *relaycommon.AutoAttemptResponseWriter) bool {
	if staged != nil {
		return staged.Committed()
	}
	return c != nil && c.Writer != nil && c.Writer.Written()
}

// autoModelAnswerOutcomeUnknown reports a delivered auto answer that the
// observation layer could not classify. It must stay neutral: recording it as a
// success would keep routing traffic to a route whose answer nobody verified.
func autoModelAnswerOutcomeUnknown(c *gin.Context, info *relaycommon.RelayInfo, staged *relaycommon.AutoAttemptResponseWriter) bool {
	if !isAutoModelRequest(c) || info == nil || staged == nil {
		return false
	}
	return !info.ClientAnswerHasValidOutput() && info.ClientAnswerUndetermined()
}

func autoModelInvalidSwitchBlockedReason(c *gin.Context, info *relaycommon.RelayInfo, budget autoModelAttemptBudget, staged *relaycommon.AutoAttemptResponseWriter) string {
	if staged == nil {
		return "staging_unavailable"
	}
	if staged.Committed() {
		return "response_committed"
	}
	if !budget.canAttempt() {
		return "budget_exhausted"
	}
	return autoModelRetryBlockedReason(c, info)
}

func autoModelUnusableFailureClass(reason string) string {
	switch {
	case strings.Contains(reason, "告警横幅"):
		return "banner_only"
	case strings.Contains(reason, "显式协议错误"):
		return "protocol_error"
	case strings.Contains(reason, "未返回任何响应内容"):
		return "empty_stream"
	case strings.Contains(reason, "响应正文为空"):
		return "empty_body"
	default:
		return "unusable"
	}
}

func newAutoModelUnusableAnswerError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New("auto model candidates returned HTTP 200 without a usable answer"),
		types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
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
	// 派发前用最新候选白名单+模型权重再复验一次:规划快照与这里之间,管理员
	// 可能已经改了候选配置或把模型权重置零。首次绑定与每次重试共用这同一道边界。
	if !service.ValidateAutoModelRouteCandidate(route.Group, route.ModelName) {
		return nil, newAutoModelRouteError("auto model route is no longer an eligible candidate")
	}
	if canceledErr := autoModelCanceledRequestError(c, nil); canceledErr != nil {
		return nil, canceledErr
	}
	if service.GetAutoModelHardExcludedChannelIDs(c)[route.ChannelID] {
		if service.IsChannelRPMLimitSkipped(c, route.ChannelID) {
			return nil, service.NewChannelRPMLimitError(service.ChannelRPMGroupLimitExceededMessage)
		}
		if service.GetChannelDailySuccessLimitSkippedIDs(c)[route.ChannelID] {
			return nil, newChannelDailySuccessLimitError()
		}
		return nil, newAutoModelRouteError("auto model channel excluded after upstream failure")
	}
	if attemptedAutoModelRoutes(c)[route] {
		return nil, newAutoModelRouteError("auto model route was already attempted")
	}
	if service.IsAutoModelModelFailed(c, route.Group, route.ModelName) {
		return nil, newAutoModelRouteError("auto model candidate was already rejected for this request")
	}
	// 授权复验必须用绕过缓存的强新鲜查询:这里是"该渠道还挂着这个模型吗"的
	// 逐次尝试正确性边界,规划路径的 TTL 快照不承担这个职责。
	targets, err := model.GetAutoModelRoutingTargetsFresh(route.Group)
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
//
// 重排后再前进,而不是在 middleware 那次的冻结计划上 index+1:计划里的候选集是
// 固定的(每个可用的 模型×渠道 各出现一次),重排只改变顺序,不会丢路。于是
// "还没试过的最优路"会自然落到队首;而冻结计划可能把它排在这 4 次预算之外,
// 让 auto 一路撞着已知很差的组合把预算用光。
// trySwitchAutoModel is the compatibility entry used by older tests and callers
// that do not need switch diagnostics.
func trySwitchAutoModel(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam, tokens int, meta *types.TokenCountMeta) bool {
	return trySwitchAutoModelWithReason(c, info, retryParam, tokens, meta, "retry", autoModelAttemptBudget{})
}

func trySwitchAutoModelWithReason(c *gin.Context, info *relaycommon.RelayInfo, retryParam *service.RetryParam, tokens int, meta *types.TokenCountMeta, switchReason string, budget autoModelAttemptBudget) bool {
	if !canRetryAutoModelRequest(c, info) || info == nil || meta == nil {
		blocked := autoModelRetryBlockedReason(c, info)
		if blocked == "" && info == nil {
			blocked = "relay_info_missing"
		}
		if blocked == "" && meta == nil {
			blocked = "token_meta_missing"
		}
		logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=%s failure=%s attempt=%d max=%d remaining=%d",
			blocked, switchReason, budget.attempts,
			operation_setting.AutoModelMaxAttempts(), budget.remainingRetries()))
		return false
	}
	current, ok := service.GetCurrentAutoModelRoute(c)
	if !ok {
		logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=no_current_route failure=%s attempt=%d max=%d",
			switchReason, budget.attempts, operation_setting.AutoModelMaxAttempts()))
		return false
	}
	next := common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex) + 1
	// 重排同时排除"已经打过"与"派发前就被本地否掉"的路线:前者防止重复上游尝试,
	// 后者防止同一条失效路线被反复放回队首。
	excludedRoutes := make(map[service.AutoModelRoute]bool)
	for route := range attemptedAutoModelRoutes(c) {
		excludedRoutes[route] = true
	}
	for route := range locallyRejectedAutoModelRoutes(c) {
		excludedRoutes[route] = true
	}
	// A used group is the outer boundary when cross-group retry is off. Rebuilding
	// from the request-level group would re-run "first nonempty group" and could
	// replace this group's untried routes with another group's plan, which dispatch
	// then correctly denies — losing the remaining same-group candidates.
	rebuildGroup := common.GetContextKeyString(c, constant.ContextKeyAutoModelRequestGroup)
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry) &&
		info.UsingGroup != "" && info.UsingGroup != "auto" {
		rebuildGroup = info.UsingGroup
	}
	routes, rebuildStatus := service.RebuildAutoModelRoutePlanForRetry(c, rebuildGroup, excludedRoutes)
	switch rebuildStatus {
	case service.AutoModelRebuildReplaced:
		service.ReplaceAutoModelRoutePlan(c, routes.Routes)
		service.SetAutoModelRoutePlanMeta(c, routes.Meta)
		next = 0
	case service.AutoModelRebuildEmpty:
		// 重排后确无候选:清空计划并结束,绝不回退旧路线。
		d := routes.Meta.Diagnostics
		service.ReplaceAutoModelRoutePlan(c, nil)
		service.SetAutoModelRoutePlanMeta(c, service.AutoModelRoutePlanMeta{})
		logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=plan_empty failure=%s attempt=%d max=%d remaining=%d cooling=%d route_excluded=%d channel=%d model=%d unauthorized=%d whitelist=%d no_targets=%d zero_weight=%d billing_endpoint=%d %s",
			switchReason, budget.attempts, operation_setting.AutoModelMaxAttempts(), budget.remainingRetries(),
			d.Cooling, d.RouteExcluded, d.ChannelExcluded, d.ModelExcluded, d.Unauthorized,
			d.WhitelistExcluded, d.NoTargets, d.ZeroWeight, d.BillingOrEndpoint,
			service.AutoModelPlanDiagnostic(routes, rebuildGroup)))
		return false
	case service.AutoModelRebuildFailed:
		// 刷新失败:停止继续派发,让调用方保留真实最后上游错误。
		logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=rebuild_failed failure=%s attempt=%d max=%d remaining=%d %s",
			switchReason, budget.attempts, operation_setting.AutoModelMaxAttempts(), budget.remainingRetries(),
			service.AutoModelPlanDiagnostic(routes, rebuildGroup)))
		return false
	default:
		// AutoModelRebuildSkipped:没有已尝试路线,继续现计划单调前进。
	}
	channel, err := prepareAutoModelRouteFromIndex(c, info, retryParam, tokens, meta, next, true)
	if err != nil {
		logger.LogInfo(c, fmt.Sprintf("auto route diag: event=no_switch reason=prepare_failed failure=%s detail=%s attempt=%d max=%d remaining=%d",
			switchReason, autoModelTraceText(err.Error(), 160), budget.attempts, operation_setting.AutoModelMaxAttempts(), budget.remainingRetries()))
		return false
	}
	planMeta := service.GetAutoModelRoutePlanMeta(c)
	d := planMeta.Diagnostics
	attempted := len(attemptedAutoModelRoutes(c))
	localRejected := len(locallyRejectedAutoModelRoutes(c))
	logger.LogInfo(c, fmt.Sprintf("auto route diag: event=switch reason=%s from=%s/%s#%d to=%s/%s#%d attempt=%d max=%d remaining=%d attempted=%d local_rejected=%d exclusions(route=%d cooling=%d channel=%d model=%d unauthorized=%d whitelist=%d no_targets=%d zero_weight=%d billing_endpoint=%d) main=%d backup=%d trial=%d group=%s %s",
		switchReason,
		current.Group, current.ModelName, current.ChannelID,
		info.UsingGroup, info.OriginModelName, channel.Id,
		budget.attempts, operation_setting.AutoModelMaxAttempts(), budget.remainingRetries(),
		attempted, localRejected,
		d.RouteExcluded, d.Cooling, d.ChannelExcluded, d.ModelExcluded, d.Unauthorized,
		d.WhitelistExcluded, d.NoTargets, d.ZeroWeight, d.BillingOrEndpoint,
		d.Main, d.Backup, d.Trial,
		rebuildGroup,
		service.AutoModelPlanDiagnostic(service.AutoModelRoutePlan{
			Routes: service.GetAutoModelRoutePlan(c),
			Meta:   planMeta,
		}, rebuildGroup)))
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
			// 派发前被拒的路线进入本地跳过集合:重排不会把它放回队首。
			if !markAutoModelRouteLocallyRejected(c, routes[index]) {
				return nil, newAutoModelRouteError("auto model local route rejections exceeded the per-request bound")
			}
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
	planMeta := service.GetAutoModelRoutePlanMeta(c)
	if category := planMeta.Categories[route]; category != "" {
		logger.LogDebug(c, "auto route: %s/%s#%d category=%s reason=%s explored=%t ratio=%.3f cold_start=%t",
			route.Group, route.ModelName, route.ChannelID, category,
			planMeta.Reasons[route], planMeta.Explored && planMeta.ExplorationRoute == route,
			planMeta.ExplorationRatio, planMeta.ColdStart)
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
// shouldRecordAutoHealthFeedback 回答"这次请求的成败要不要并入 auto 健康体系"。
//   - auto 请求:始终记录(原有语义);
//   - 具名请求(固定模型):同样记录——成败都是 (分组,模型,渠道) 的证据,差异只在
//     路由行为(auto 失败会换路由,具名不会);需要 AutoModelEnabled 开启。
//
// 两种形态的共同前提:本次尝试真的进入了上游派发,且客户端没有提前离场。
// 记录不影响任何路由行为:具名请求的重试与渠道选择维持原有逻辑。
func shouldRecordAutoHealthFeedback(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if info == nil || !info.HasUpstreamDispatch() || autoModelClientAbandoned(c, info) {
		return false
	}
	if isAutoModelRequest(c) {
		return true
	}
	return operation_setting.AutoModelEnabled
}

func recordAutoModelFeedback(c *gin.Context, info *relaycommon.RelayInfo, success bool) {
	if !shouldRecordAutoHealthFeedback(c, info) {
		return
	}
	latencyMS := observedAutoLatency(info, success)
	queueAutoModelFeedback(c, info, autoModelFeedback{
		success:      success,
		stream:       info.IsStream,
		latencyKnown: latencyMS > 0,
		latencyMS:    latencyMS,
		elapsedMS:    observedAutoModelCooldownLatency(info, success),
	})
}

// recordAutoModelUpstreamFailure 记录一次上游失败,并在入队时定格它的"永久性":
// 命中确定性关键词的失败重试也不会自愈,要的是直接摘除该组合,而不是冷却一段
// 时间后再来一次。分类必须现在做,不能等 flush(那时 RelayInfo 可能已经
// 是另一次尝试的字段)。
//
// 失败也带上耗时:一个 200ms 就被拒的组合和一个卡满 60 秒才失败的组合,对客户端
// 是两种体验。早先失败一律 latencyMS=0,评分里两者的延迟惩罚完全相同,耗着不
// 说话的那个反而被当成"响应快"。耗时走和成功相同的 EWMA,所以它只影响评分排序,
// 不改变冷却判据。
func recordAutoModelUpstreamFailure(c *gin.Context, info *relaycommon.RelayInfo, err *types.NewAPIError) {
	if !shouldRecordAutoHealthFeedback(c, info) {
		return
	}
	elapsedMS := observedAutoModelCooldownLatency(info, false)
	queueAutoModelFeedback(c, info, autoModelFeedback{
		success:   false,
		stream:    info.IsStream,
		latencyMS: elapsedMS,
		elapsedMS: elapsedMS,
		permanent: service.AutoModelPermanentFailure(err),
		reason:    service.AutoModelFailureReason(err),
	})
}

// recordAutoModelStreamFailure 记录一次"已交付部分内容、但上游流异常结束"的失败。
// 这条路径没有 NewAPIError（不能再给客户端写第二个响应），所以只影响评分与冷却。
func recordAutoModelStreamFailure(c *gin.Context, info *relaycommon.RelayInfo, reason string) {
	if !shouldRecordAutoHealthFeedback(c, info) {
		return
	}
	elapsedMS := observedAutoModelCooldownLatency(info, false)
	queueAutoModelFeedback(c, info, autoModelFeedback{
		success:   false,
		stream:    info.IsStream,
		latencyMS: elapsedMS,
		elapsedMS: elapsedMS,
		reason:    reason,
	})
}

// An unusable HTTP 200 is the final failure of this combination, not a success.
func recordAutoModelUnusableAnswer(c *gin.Context, info *relaycommon.RelayInfo, reason string) {
	if !shouldRecordAutoHealthFeedback(c, info) {
		return
	}
	if err := info.UpstreamProtocolError(); err != nil && service.AutoModelPermanentFailure(err) {
		// A 200 SSE envelope must not hide a configured deterministic rejection.
		// Pin the decision now, before a retry resets this attempt's observer.
		queueAutoModelFeedback(c, info, autoModelFeedback{
			stream: info.IsStream, permanent: true, reason: service.AutoModelFailureReason(err),
		})
		return
	}
	queueAutoModelFeedback(c, info, autoModelFeedback{stream: info.IsStream, unusable: true, reason: reason})
}

// autoModelClientAbandoned 判断"客户端在拿到本次交付物之前就离开了"。
//
// 不能只看 c.Request.Context().Err()：客户端收完正文后正常关闭连接（curl、SDK、
// 下位机都是收到完整响应立即断开）时 ctx 同样是 canceled，于是每个正常完成的请求
// 都会被判成"客户端断开"，全部 auto 反馈被丢弃——分数不更新、冷却也无法记录。
// 流明确以 client_gone 结束时，即使已交付部分内容也属于客户端中断；正常收尾
// 后的取消仍保留反馈。没有流结束信号时，ctx 取消只在尚未写入时作为离场证据。
func autoModelClientAbandoned(c *gin.Context, info *relaycommon.RelayInfo) bool {
	if info != nil && info.ClientDeliveryBroken() {
		return true
	}
	if info != nil && info.StreamStatus != nil {
		status := info.StreamStatus
		if status.EndReason == relaycommon.StreamEndReasonClientGone &&
			!errors.Is(status.EndError, relaycommon.ErrAutoExplorationTimeout) {
			return true
		}
		// Older/custom scanners can report a canceled upstream read before the
		// client-gone branch wins. Require the live request's cancellation too;
		// a read failure on an active request remains upstream evidence.
		if status.EndReason == relaycommon.StreamEndReasonScannerErr &&
			c != nil && c.Request != nil && c.Request.Context().Err() != nil &&
			(errors.Is(status.EndError, context.Canceled) || errors.Is(status.EndError, context.DeadlineExceeded)) {
			return true
		}
	}
	if autoModelRequestContextError(c) == nil {
		return false
	}
	// stream ended: reason=done means the upstream completed normally. In this
	// case a zero-byte delivery is an upstream empty response, not a client
	// disconnect; let ClientAnswerUnusable classify it and cool the auto route.
	if info != nil && info.UpstreamContentExhausted() {
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
	sequence     int // order of this key's last observation in the request
	success      bool
	stream       bool
	latencyKnown bool
	latencyMS    int64
	elapsedMS    int64 // cooldown signal: successful TTFB (or non-stream elapsed), failed elapsed
	unusable     bool
	reason       string
	// observedAt 在入队时固定为这次尝试的真实观测时间。flush 可能发生在后续
	// 长请求之后,不能用 flush 时间刷新早先观测的新鲜度。
	observedAt time.Time
	// permanent 在入队时定格:这次失败是否命中管理员配置的确定性失败关键词。
	// 队列是延迟 flush 的,不能等到 flush 时再读可能已被复用/覆盖的 RelayInfo。
	permanent bool
}

const autoModelFeedbackContextKey = "auto_model_feedback_pending"
const autoModelTrialSlotsContextKey = "auto_model_trial_slots"

func holdAutoModelTrialSlot(c *gin.Context, route service.AutoModelRoute, release func()) {
	if c == nil || release == nil {
		return
	}
	value, _ := c.Get(autoModelTrialSlotsContextKey)
	slots, _ := value.(map[service.AutoModelRoute]func())
	if slots == nil {
		slots = make(map[service.AutoModelRoute]func())
	}
	slots[route] = release
	c.Set(autoModelTrialSlotsContextKey, slots)
}

func releaseAutoModelTrialSlot(c *gin.Context, route service.AutoModelRoute) {
	if c == nil {
		return
	}
	value, _ := c.Get(autoModelTrialSlotsContextKey)
	slots, _ := value.(map[service.AutoModelRoute]func())
	release, ok := slots[route]
	if !ok {
		return
	}
	delete(slots, route)
	c.Set(autoModelTrialSlotsContextKey, slots)
	release()
}

func releaseAllAutoModelTrialSlots(c *gin.Context) {
	if c == nil {
		return
	}
	value, _ := c.Get(autoModelTrialSlotsContextKey)
	slots, _ := value.(map[service.AutoModelRoute]func())
	c.Set(autoModelTrialSlotsContextKey, nil)
	for _, release := range slots {
		release()
	}
}

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
	// Last attempt wins, including its position: a replacement must be applied
	// after observations of other models that happened before this attempt.
	feedback.sequence = 1
	for _, previous := range pending {
		feedback.sequence = max(feedback.sequence, previous.sequence+1)
	}
	feedback.observedAt = time.Now()
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
	pending := takeAutoModelFeedback(c)
	keys := make([]autoModelFeedbackKey, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	// 反馈必须按"本次请求内的观察顺序"应用:每个键只保留最后一条(替换语义),
	// 而替换发生的时间点也要排在其他键的观察之后。绝不能按 map 迭代顺序乱序喂给
	// 评分与冷却。
	sort.Slice(keys, func(i, j int) bool {
		return pending[keys[i]].sequence < pending[keys[j]].sequence
	})
	for _, key := range keys {
		feedback := pending[key]
		switch {
		case feedback.unusable:
			service.RecordAutoModelUnusableAnswerAt(key.group, key.modelName, key.channelID, feedback.reason, feedback.observedAt)
		case feedback.permanent:
			// 确定性失败:直接把这个模型从这条渠道里摘除(渠道无剩余模型时自动
			// 禁用),没有冷却等待;仍然只挡这一个组合。
			service.RecordAutoModelPermanentFailureAt(key.group, key.modelName, key.channelID, feedback.reason, feedback.observedAt)
		default:
			service.RecordAutoModelOutcomeWithModeAt(key.group, key.modelName, key.channelID,
				feedback.success, feedback.stream, feedback.latencyKnown,
				feedback.latencyMS, feedback.elapsedMS, feedback.reason, feedback.observedAt)
		}
		feedbackAction := "failure_cooldown"
		if feedback.success {
			feedbackAction = "success_recovery"
		}
		if feedback.unusable {
			feedbackAction = "unusable_cooldown"
		}
		if feedback.permanent {
			feedbackAction = "retirement_requested"
		}
		logger.LogInfo(c, fmt.Sprintf("auto route diag: event=feedback group=%s model=%s channel=%d action=%s observed_at=%s publish_delay_ms=%d stream=%t latency_known=%t latency_ms=%d %s",
			autoModelTraceText(key.group, 80), autoModelTraceText(key.modelName, 100), key.channelID, feedbackAction,
			feedback.observedAt.UTC().Format(time.RFC3339Nano), max(int64(0), time.Since(feedback.observedAt).Milliseconds()),
			feedback.stream, feedback.latencyKnown, feedback.latencyMS,
			service.AutoModelFeedbackDiagnostic(service.AutoModelRoute{Group: key.group, ModelName: key.modelName, ChannelID: key.channelID}, feedback.stream)))
		// The observation is now published; only now may another request use the
		// same trial route.
		releaseAutoModelTrialSlot(c, service.AutoModelRoute{
			Group: key.group, ModelName: key.modelName, ChannelID: key.channelID,
		})
	}
	// Slots whose attempt produced no feedback (pre-dispatch/local rejection or
	// client abandonment) are safe to release once the request has finished.
	releaseAllAutoModelTrialSlots(c)
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
	recordRelayErrorLog(c, err)
}

// Record diagnostics without changing client delivery, billing or channel health.
func recordRelayErrorLog(c *gin.Context, err *types.NewAPIError) {
	// 无条件记录失败请求:早先这里还有一道 constant.ErrorLogEnabled 门控
	// (ERROR_LOG_ENABLED,默认 false),后果是"报错的同时进入了冷却",但面板
	// 「错误日志」里一条都看不到——排查时只看到渠道被冷却,看不到原因。
	// IsRecordErrorLog 仍保留:少数明确标注"不要记日志"的错误(如预扣费失败、
	// RPM 限流)依然不写库,避免噪声淹没真正的上游失败。
	if types.IsRecordErrorLog(err) {
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

// logModelRequestError 将失败的模型请求写入本地按天分割的报错日志文件。
// 若本次失败已触发冷却,一并写出冷却等级。
func logModelRequestError(c *gin.Context, err *types.NewAPIError) {
	modelName := c.GetString("original_model")
	cooldown := ""
	if relayInfo := relaycommon.GetRelayInfo(c); relayInfo != nil {
		if relayInfo.IsModelMappingFullActive() {
			modelName = relayInfo.GetDisplayModelName()
		}
		// 冷却粒度与打分一致:(分组, 原始模型名, 渠道)。本地报错日志在
		// flushAutoModelFeedback 之后写入,此时本次触发的冷却已经生效,可直接读等级。
		var cooldownChannel int
		if relayInfo.ChannelMeta != nil {
			cooldownChannel = relayInfo.ChannelMeta.ChannelId
		}
		if info, ok := service.GetAutoModelCooldown(relayInfo.UsingGroup, relayInfo.OriginModelName, cooldownChannel, time.Now()); ok {
			cooldown = fmt.Sprintf(", cooldown level=%d", info.Level)
		}
	}
	logger.LogModelError(c, fmt.Sprintf("model request failed: model=%s, channel=%d, status=%d, error=%s%s",
		modelName, c.GetInt("channel_id"), err.StatusCode, err.MaskSensitiveErrorWithStatusCode(), cooldown))
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
	if info.IsStream {
		// Only the first actual answer delta is a stream success-speed sample.
		// First byte/heartbeat/metadata and a fast partial stream that later fails
		// must not look fast.
		if latency, ok := info.AttemptFirstValidOutputLatency(); ok {
			return max(int64(1), latency.Milliseconds())
		}
		return 0
	}
	// 非流式成功速度从真实派发开始计算，排除参数转换、图片下载与编码；
	// 请求总耗时仍由 AttemptElapsed 供日志使用。
	duration := info.AttemptDispatchElapsed()
	if duration <= 0 {
		return 0
	}
	return max(int64(1), duration.Milliseconds())
}

func observedAutoModelCooldownLatency(info *relaycommon.RelayInfo, success bool) int64 {
	if success {
		return observedAutoLatency(info, true)
	}
	return observedAutoModelElapsed(info)
}

const autoModelAttemptTraceKey = "auto_model_attempt_trace"

// autoModelAttemptTraceEntry is a request-local, log-only record of one dispatched
// upstream attempt. It never feeds routing decisions; the stop log prints it so a
// single request shows which routes were tried, how long each took and why it
// ended.
type autoModelAttemptTraceEntry struct {
	Index             int
	Route             service.AutoModelRoute
	Category          string
	Outcome           string
	ElapsedMS         int64
	TTFTMS            int64
	DispatchElapsedMS int64
	Detail            string
}

func traceAutoModelAttempt(c *gin.Context, info *relaycommon.RelayInfo, outcome, detail string) {
	if c == nil || info == nil || !info.HasUpstreamDispatch() {
		return
	}
	route, ok := service.GetCurrentAutoModelRoute(c)
	if !ok {
		return
	}
	category := service.GetAutoModelRoutePlanMeta(c).Categories[route]
	ttft := int64(-1)
	if latency, known := info.AttemptFirstValidOutputLatency(); known {
		ttft = latency.Milliseconds()
	}
	entries := autoModelAttemptTrace(c)
	entries = append(entries, autoModelAttemptTraceEntry{
		Index:             len(entries) + 1,
		Route:             route,
		Category:          category,
		Outcome:           outcome,
		ElapsedMS:         info.AttemptElapsed().Milliseconds(),
		TTFTMS:            ttft,
		DispatchElapsedMS: info.AttemptDispatchElapsed().Milliseconds(),
		Detail:            autoModelTraceText(detail, 80),
	})
	c.Set(autoModelAttemptTraceKey, entries)
	entry := entries[len(entries)-1]
	ttftLog := "?"
	if entry.TTFTMS >= 0 {
		ttftLog = fmt.Sprintf("%d", entry.TTFTMS)
	}
	logger.LogInfo(c, fmt.Sprintf("auto route diag: event=result attempt=%d real_dispatched=%d group=%s model=%s channel=%d category=%s outcome=%s elapsed_ms=%d dispatch_elapsed_ms=%d first_valid_output_ms=%s stream=%t written=%t %s detail=%q route={%s}",
		info.RetryIndex+1, entry.Index, autoModelTraceText(route.Group, 80), autoModelTraceText(route.ModelName, 100), route.ChannelID,
		category, outcome, entry.ElapsedMS, entry.DispatchElapsedMS, ttftLog, info.IsStream, c.Writer != nil && c.Writer.Written(),
		autoModelRequestDeadlineLog(c), entry.Detail, service.AutoModelRouteDiagnostic(service.GetAutoModelRoutePlanMeta(c), route)))
}

func autoModelAttemptTrace(c *gin.Context) []autoModelAttemptTraceEntry {
	if c == nil {
		return nil
	}
	value, _ := c.Get(autoModelAttemptTraceKey)
	entries, _ := value.([]autoModelAttemptTraceEntry)
	return entries
}

func formatAutoModelAttemptTrace(c *gin.Context) string {
	entries := autoModelAttemptTrace(c)
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		category := entry.Category
		if category == "" {
			category = "-"
		}
		ttft := "?"
		if entry.TTFTMS >= 0 {
			ttft = fmt.Sprintf("%dms", entry.TTFTMS)
		}
		part := fmt.Sprintf("#%d %s/%s#%d cat=%s outcome=%s elapsed=%dms ttft=%s dispatch_elapsed=%dms",
			entry.Index, autoModelTraceText(entry.Route.Group, 80), autoModelTraceText(entry.Route.ModelName, 100), entry.Route.ChannelID,
			category, entry.Outcome, entry.ElapsedMS, ttft, entry.DispatchElapsedMS)
		if entry.Detail != "" {
			part += " detail=" + entry.Detail
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}

func autoModelTraceText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}
