package command

import (
	"context"
	"github.com/xtls/xray-core/app/router"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features"
	"github.com/xtls/xray-core/features/routing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *routingServer) controller() core.RuntimeController {
	if s.instance == nil {
		return nil
	}
	return s.instance.RuntimeController()
}

func (s *routingServer) withRouter(ctx context.Context, callback func(*routingServer) error) error {
	if controller := s.controller(); controller != nil {
		return controller.View(ctx, routing.RouterType(), func(feature features.Feature) error {
			view := *s
			view.instance, view.router = nil, feature.(routing.Router)
			return callback(&view)
		})
	}
	return callback(s)
}

func (s *routingServer) GetBalancerInfo(ctx context.Context, request *GetBalancerInfoRequest) (response *GetBalancerInfoResponse, err error) {
	err = s.withRouter(ctx, func(view *routingServer) error { response, err = view.getBalancerInfo(ctx, request); return err })
	return
}
func (s *routingServer) OverrideBalancerTarget(ctx context.Context, request *OverrideBalancerTargetRequest) (response *OverrideBalancerTargetResponse, err error) {
	err = s.withRouter(ctx, func(view *routingServer) error { response, err = view.overrideBalancerTarget(ctx, request); return err })
	return
}
func (s *routingServer) ListRule(ctx context.Context, request *ListRuleRequest) (response *ListRuleResponse, err error) {
	err = s.withRouter(ctx, func(view *routingServer) error { response, err = view.listRule(ctx, request); return err })
	return
}
func (s *routingServer) TestRoute(ctx context.Context, request *TestRouteRequest) (response *RoutingContext, err error) {
	err = s.withRouter(ctx, func(view *routingServer) error { response, err = view.testRoute(ctx, request); return err })
	return
}

func editRouting(config *core.Config, edit func(*router.Config) error) error {
	for i, app := range config.App {
		if app.Type != serial.GetMessageType(&router.Config{}) {
			continue
		}
		message, err := app.GetInstance()
		if err != nil {
			return err
		}
		rules := message.(*router.Config)
		if err := edit(rules); err != nil {
			return err
		}
		config.App[i] = serial.ToTypedMessage(rules)
		return nil
	}
	rules := new(router.Config)
	if err := edit(rules); err != nil {
		return err
	}
	config.App = append(config.App, serial.ToTypedMessage(rules))
	return nil
}

func (s *routingServer) AddRule(ctx context.Context, request *AddRuleRequest) (*AddRuleResponse, error) {
	if controller := s.controller(); controller != nil {
		if request.Config == nil {
			return nil, status.Error(codes.InvalidArgument, "Routing settings are required")
		}
		message, err := request.Config.GetInstance()
		if err != nil {
			return nil, err
		}
		added, ok := message.(*router.Config)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "Invalid routing settings")
		}
		return &AddRuleResponse{}, controller.Update(ctx, func(config *core.Config) error {
			return editRouting(config, func(rules *router.Config) error {
				added := proto.Clone(added).(*router.Config)
				if !request.ShouldAppend {
					rules.Rule, rules.BalancingRule = nil, nil
				}
				rules.Rule = append(rules.Rule, added.Rule...)
				rules.BalancingRule = append(rules.BalancingRule, added.BalancingRule...)
				return nil
			})
		})
	}
	return s.addRule(ctx, request)
}

func (s *routingServer) RemoveRule(ctx context.Context, request *RemoveRuleRequest) (*RemoveRuleResponse, error) {
	if controller := s.controller(); controller != nil {
		if request.RuleTag == "" {
			return nil, status.Error(codes.InvalidArgument, "Rule tag is required")
		}
		return &RemoveRuleResponse{}, controller.Update(ctx, func(config *core.Config) error {
			return editRouting(config, func(rules *router.Config) error {
				kept := rules.Rule[:0]
				for _, rule := range rules.Rule {
					if rule.RuleTag != request.RuleTag {
						kept = append(kept, rule)
					}
				}
				rules.Rule = kept
				return nil
			})
		})
	}
	return s.removeRule(ctx, request)
}
