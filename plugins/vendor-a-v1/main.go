// 厂商 A 插件 v1：调 A 的 v1 接口 {"title","amount"} 并映射成统一结构。
package main

import (
	"context"
	"net/url"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

func main() {
	pluginapi.Serve("A", "v1", func(ctx context.Context, base string, req pluginapi.QueryRequest) (pluginapi.QueryResponse, error) {
		var v struct {
			Title  string `json:"title"`
			Amount string `json:"amount"`
		}
		if err := pluginapi.GetJSON(ctx, base+"/vendorA/v1/items/"+url.PathEscape(req.ID)+pluginapi.DelayQuery(req.DelayMS), &v); err != nil {
			return pluginapi.QueryResponse{}, err
		}
		cents, err := pluginapi.ParseDecimalCents(v.Amount)
		if err != nil {
			return pluginapi.QueryResponse{}, err
		}
		return pluginapi.QueryResponse{ID: req.ID, Name: v.Title, PriceCents: cents}, nil
	})
}
