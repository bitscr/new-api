package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/console_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/gin-gonic/gin"
)

var completionRatioMetaOptionKeys = []string{
	"ModelPrice",
	"ModelRatio",
	"CompletionRatio",
	"CacheRatio",
	"CreateCacheRatio",
	"ImageRatio",
	"AudioRatio",
	"AudioCompletionRatio",
}

func isVisiblePublicKeyOption(key string) bool {
	switch key {
	case "WaffoPancakeWebhookPublicKey", "WaffoPancakeWebhookTestKey":
		return true
	default:
		return false
	}
}

func collectModelNamesFromOptionValue(raw string, modelNames map[string]struct{}) {
	if strings.TrimSpace(raw) == "" {
		return
	}

	var parsed map[string]any
	if err := common.UnmarshalJsonStr(raw, &parsed); err != nil {
		return
	}

	for modelName := range parsed {
		modelNames[modelName] = struct{}{}
	}
}

func buildCompletionRatioMetaValue(optionValues map[string]string) string {
	modelNames := make(map[string]struct{})
	for _, key := range completionRatioMetaOptionKeys {
		collectModelNamesFromOptionValue(optionValues[key], modelNames)
	}

	meta := make(map[string]ratio_setting.CompletionRatioInfo, len(modelNames))
	for modelName := range modelNames {
		meta[modelName] = ratio_setting.GetCompletionRatioInfo(modelName)
	}

	jsonBytes, err := common.Marshal(meta)
	if err != nil {
		return "{}"
	}
	return string(jsonBytes)
}

func GetOptions(c *gin.Context) {
	var options []*model.Option
	optionValues := make(map[string]string)
	common.OptionMapRWMutex.Lock()
	for k, v := range common.OptionMap {
		value := common.Interface2String(v)
		isSensitiveKey := strings.HasSuffix(k, "Token") ||
			strings.HasSuffix(k, "Secret") ||
			strings.HasSuffix(k, "Key") ||
			strings.HasSuffix(k, "secret") ||
			strings.HasSuffix(k, "api_key")
		if isSensitiveKey && !isVisiblePublicKeyOption(k) {
			continue
		}
		options = append(options, &model.Option{
			Key:   k,
			Value: value,
		})
		for _, optionKey := range completionRatioMetaOptionKeys {
			if optionKey == k {
				optionValues[k] = value
				break
			}
		}
	}
	common.OptionMapRWMutex.Unlock()
	options = append(options, &model.Option{
		Key:   "CompletionRatioMeta",
		Value: buildCompletionRatioMetaValue(optionValues),
	})
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    options,
	})
	return
}

type OptionUpdateRequest struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func UpdateOption(c *gin.Context) {
	var option OptionUpdateRequest
	// suggestion 在"值已保存、但组合不合逻辑"时带上建议值,随成功响应返回,
	// 前端显示成"保存成功,但建议……"。它不阻止保存。
	var suggestion string
	err := common.DecodeJson(c.Request.Body, &option)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "无效的参数",
		})
		return
	}
	switch option.Value.(type) {
	case bool:
		option.Value = common.Interface2String(option.Value.(bool))
	case float64:
		option.Value = common.Interface2String(option.Value.(float64))
	case int:
		option.Value = common.Interface2String(option.Value.(int))
	default:
		option.Value = fmt.Sprintf("%v", option.Value)
	}
	switch option.Key {
	case "nodeloc.enabled":
		if option.Value != "true" && option.Value != "false" {
			common.ApiErrorI18n(c, i18n.MsgInvalidParams)
			return
		}
		if option.Value == "true" && !system_setting.GetNodeLocSettings().IsConfigured() {
			common.ApiErrorI18n(c, i18n.MsgNodeLocConfigInvalid)
			return
		}
	case "nodeloc.client_secret":
		if strings.TrimSpace(option.Value.(string)) == "" {
			c.JSON(http.StatusOK, gin.H{"success": true, "message": ""})
			return
		}
	case "nodeloc.client_id":
		option.Value = strings.TrimSpace(option.Value.(string))
		if option.Value == "" && system_setting.GetNodeLocSettings().Enabled {
			common.ApiErrorI18n(c, i18n.MsgNodeLocConfigInvalid)
			return
		}
	case "ServerAddress":
		if system_setting.GetNodeLocSettings().Enabled {
			if _, err := system_setting.NodeLocRedirectURI(option.Value.(string)); err != nil {
				common.ApiErrorI18n(c, i18n.MsgNodeLocConfigInvalid)
				return
			}
		}
	case "GitHubOAuthEnabled":
		if option.Value == "true" && common.GitHubClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 GitHub OAuth，请先填入 GitHub Client Id 以及 GitHub Client Secret！",
			})
			return
		}
	case "GitHubMinimumAccountAgeSeconds":
		minimumAgeSeconds, parseErr := strconv.ParseInt(option.Value.(string), 10, 64)
		if parseErr != nil || minimumAgeSeconds < 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "GitHub 账号年龄限制必须是非负整数秒",
			})
			return
		}
	case "discord.enabled":
		if option.Value == "true" && system_setting.GetDiscordSettings().ClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Discord OAuth，请先填入 Discord Client Id 以及 Discord Client Secret！",
			})
			return
		}
	case "oidc.enabled":
		if option.Value == "true" && system_setting.GetOIDCSettings().ClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 OIDC 登录，请先填入 OIDC Client Id 以及 OIDC Client Secret！",
			})
			return
		}
	case "LinuxDOOAuthEnabled":
		if option.Value == "true" && common.LinuxDOClientId == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 LinuxDO OAuth，请先填入 LinuxDO Client Id 以及 LinuxDO Client Secret！",
			})
			return
		}
	case "EmailDomainRestrictionEnabled":
		if option.Value == "true" && len(common.EmailDomainWhitelist) == 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用邮箱域名限制，请先填入限制的邮箱域名！",
			})
			return
		}
	case "WeChatAuthEnabled":
		if option.Value == "true" && common.WeChatServerAddress == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用微信登录，请先填入微信登录相关配置信息！",
			})
			return
		}
	case "TurnstileCheckEnabled":
		if option.Value == "true" && common.TurnstileSiteKey == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Turnstile 校验，请先填入 Turnstile 校验相关配置信息！",
			})

			return
		}
	case "TelegramOAuthEnabled":
		if option.Value == "true" && common.TelegramBotToken == "" {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "无法启用 Telegram OAuth，请先填入 Telegram Bot Token！",
			})
			return
		}
	case "GroupRatio":
		err = ratio_setting.CheckGroupRatio(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "GroupDescriptions":
		err = setting.CheckGroupDescriptions(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "group_ratio_setting.group_display":
		err = ratio_setting.CheckGroupDisplay(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "ImageRatio":
		err = ratio_setting.UpdateImageRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "图片倍率设置失败: " + err.Error(),
			})
			return
		}
	case "AudioRatio":
		err = ratio_setting.UpdateAudioRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "音频倍率设置失败: " + err.Error(),
			})
			return
		}
	case "AudioCompletionRatio":
		err = ratio_setting.UpdateAudioCompletionRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "音频补全倍率设置失败: " + err.Error(),
			})
			return
		}
	case "CreateCacheRatio":
		err = ratio_setting.UpdateCreateCacheRatioByJSONString(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "缓存创建倍率设置失败: " + err.Error(),
			})
			return
		}
	case "ModelRequestRateLimitGroup":
		err = setting.CheckModelRequestRateLimitGroup(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "ModelRequestConcurrencyLimit":
		err = setting.CheckModelRequestConcurrencyLimit(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "probe_guard_setting.enabled",
		"probe_guard_setting.dry_run",
		"probe_guard_setting.window_seconds",
		"probe_guard_setting.distinct_model_count",
		"probe_guard_setting.first_ip_ban_minutes",
		"probe_guard_setting.second_ip_ban_minutes",
		"probe_guard_setting.permanent_offense_count",
		"probe_guard_setting.offense_dedupe_seconds",
		"probe_guard_setting.max_ips_per_offense",
		"probe_guard_setting.whitelist_user_ids":
		err = setting.CheckProbeGuardOption(option.Key, option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "AutomaticDisableStatusCodes":
		_, err = operation_setting.ParseHTTPStatusCodeRanges(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "AutoModelCandidates":
		// 空串与 "{}" 都表示"清空白名单,所有分组回退默认(不限制)",这是
		// SetAutoModelCandidates 明确支持的语义。早先这里无条件 Unmarshal,
		// 空串会被 JSON 解析拒掉——于是面板上把文本框删空再保存就报
		// "unexpected end of JSON input",管理员没法把白名单改回不限制。
		// 校验必须接受 setter 接受的输入。
		if trimmed := strings.TrimSpace(option.Value.(string)); trimmed != "" {
			var candidates map[string][]string
			if err = common.Unmarshal([]byte(trimmed), &candidates); err != nil {
				c.JSON(http.StatusOK, gin.H{"success": false, "message": "无效的 AutoModelCandidates JSON: " + err.Error()})
				return
			}
		}
	case "AutoModelWeights":
		if err = operation_setting.ValidateAutoModelWeights(option.Value.(string)); err != nil {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "无效的 AutoModelWeights JSON: " + err.Error()})
			return
		}
	case "AutoModelEnabled":
		if option.Value.(string) != "true" && option.Value.(string) != "false" {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "AutoModelEnabled 必须是 true 或 false"})
			return
		}
	case "AutoModelMaxAttempts", "AutoModelPermanentCooldownHours", "AutoModelCooldownMaxHours",
		"AutoModelScoreDecayMinutes":
		// 这些都是"至少 1"的正整数;写 0 或负数会直接让 auto 不可用。
		// (确定性失败已无"封顶天数"选项:首犯 24h、再犯直接摘除模型。)
		parsed, parseErr := strconv.Atoi(strings.TrimSpace(option.Value.(string)))
		if parseErr != nil || parsed < 1 {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "该选项必须是大于等于 1 的整数",
			})
			return
		}
		switch option.Key {
		case "AutoModelCooldownMaxHours":
			// 封顶不能低于首档(15 分钟),否则阶梯第一档就超封顶,语义自相矛盾。
			// 小时为整数,>=1 即满足。
			//
			// 这里只记建议、不拒绝:封顶与衰减窗口都是可自定义的,不合逻辑的组合也
			// 允许保存,只在响应里附带建议值提示(前端显示"保存成功,但建议……")。
			// 之所以不拒绝、也不替用户改值:前端把两个键放在同一批并发 PUT 里,
			// 拒绝会造成"一个被拒、另一个已生效"的半应用;而悄悄改值会让用户以为
			// 自己填的生效了。给建议既幂等又不骗人。
			if advise := operation_setting.AutoModelRecommendedCooldownMaxHours(); parsed != advise {
				suggestion = fmt.Sprintf("保存成功,但建议封顶值为 %d", advise)
			}
		case "AutoModelScoreDecayMinutes":
			// 衰减窗口不足时同样只建议、不拒绝(理由同上)。
			// 两种不合逻辑都提示:小于冷却第二档(惩罚在第 2 次解冻时归零),
			// 或小于封顶(最高几档解冻时惩罚归零)。
			recommended := operation_setting.AutoModelRecommendedScoreDecayMinutes()
			tooShort := time.Duration(parsed)*time.Minute <= service.AutoModelCooldownSecondTierForValidation()
			belowMax := parsed < operation_setting.AutoModelCooldownMaxHours()*60
			if tooShort || belowMax || parsed != recommended {
				suggestion = fmt.Sprintf("保存成功,但建议评分衰减值为 %d", recommended)
			}
		}
	case "AutoModelPermanentKeywords":
		// 关键词是"把某类失败升级为长冷却"的判据,代价高:命中即 24h 起。
		// 只做最基本的可用性校验,不猜测管理员的意图;但显然无意义或会误伤的
		// 条目必须在写库前挡住,否则一次粘贴就把一片组合封 30 天。
		for _, line := range strings.Split(option.Value.(string), "\n") {
			if message := operation_setting.ValidateAutoModelPermanentKeyword(line); message != "" {
				c.JSON(http.StatusOK, gin.H{"success": false, "message": message})
				return
			}
		}
	case "AutoModelTransientKeywords":
		// 这张表是"降级为普通冷却",写宽了不会造成封禁,只会少一次长冷却,
		// 所以只挡显然无意义的条目(过短、空行),不做额外限制。
		for _, line := range strings.Split(option.Value.(string), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if len([]rune(trimmed)) < 2 {
				c.JSON(http.StatusOK, gin.H{
					"success": false,
					"message": "关键词过短(少于 2 个字符)会误伤大量无关失败:" + trimmed,
				})
				return
			}
		}
	case "console_setting.api_info":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "ApiInfo")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.announcements":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "Announcements")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.faq":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "FAQ")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "console_setting.uptime_kuma_groups":
		err = console_setting.ValidateConsoleSettings(option.Value.(string), "UptimeKumaGroups")
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	case "site_background.config":
		err = system_setting.ValidateSiteBackgroundConfig(option.Value.(string))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
	}
	err = model.UpdateOption(option.Key, option.Value.(string))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": suggestion,
	})
	return
}
