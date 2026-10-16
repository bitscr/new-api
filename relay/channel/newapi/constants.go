package newapi

const ChannelName = "newapi"

// ModelList 为空:上游是另一个 new-api 网关,模型列表从其 /v1/models 动态拉取。
var ModelList = []string{}
