// 厂商 A 插件 v2：厂商改了接口形状 {"name","price_cents","currency"}，映射回同一个统一结构。
package main

import (
	"context"
	"net/url"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

func main() {
	pluginapi.Serve("A", "v2", func(ctx context.Context, base string, req pluginapi.QueryRequest) (pluginapi.QueryResponse, error) {
		var v struct {
			Name       string `json:"name"`
			PriceCents int64  `json:"price_cents"`
			Currency   string `json:"currency"`
		}
		if err := pluginapi.GetJSON(ctx, base+"/vendorA/v2/items/"+url.PathEscape(req.ID)+pluginapi.DelayQuery(req.DelayMS), &v); err != nil {
			return pluginapi.QueryResponse{}, err
		}
		return pluginapi.QueryResponse{ID: req.ID, Name: v.Name, PriceCents: v.PriceCents}, nil
	})
}
