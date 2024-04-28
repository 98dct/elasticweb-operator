/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	elasticwebdctcomv1 "elasticweb-operator/api/v1"
)

const (
	// deployment中的APP标签名
	APP_NAME = "elastic-app"
	// tomcat容器的端口号
	CONTAINER_PORT = 8080
	// 单个POD的CPU资源申请
	CPU_REQUEST = "100m"
	// 单个POD的CPU资源上限
	CPU_LIMIT = "100m"
	// 单个POD的内存资源申请
	MEM_REQUEST = "512Mi"
	// 单个POD的内存资源上限
	MEM_LIMIT = "512Mi"
)

var logger *zap.Logger

func init() {
	cfg := zap.Config{
		Encoding:         "console",
		Level:            zap.NewAtomicLevelAt(zapcore.InfoLevel),
		EncoderConfig:    zap.NewProductionEncoderConfig(),
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
	}

	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder   // 设置时间格式
	cfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder // 显示短路径的调用者信息

	zaplogger, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	logger = zaplogger
}

// ElasticWebReconciler reconciles a ElasticWeb object
type ElasticWebReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=elasticweb.dct.com,resources=elasticwebs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=elasticweb.dct.com,resources=elasticwebs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=elasticweb.dct.com,resources=elasticwebs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ElasticWeb object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *ElasticWebReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// TODO(user): your logic here
	logger.Sugar().Infof("elasticweb:%s\n", req.NamespacedName)

	logger.Sugar().Infof("1. start reconcile logic")

	instance := &elasticwebdctcomv1.ElasticWeb{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Sugar().Infof("instance not found")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	logger.Sugar().Infof("instance: %s\n", instance.String())

	//查找deployment
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {

		if errors.IsNotFound(err) {
			logger.Sugar().Infof("deployment not exists")

			if *(instance.Spec.TotalQPS) < 1 {
				logger.Sugar().Infof("not need deployment")
				return ctrl.Result{}, nil
			}

			//先创建svc
			if err = createServiceIfNotExist(ctx, r, instance, req); err != nil {
				return ctrl.Result{}, err
			}

			//立即创建deployment
			if err = createDeployment(ctx, r, instance); err != nil {
				return ctrl.Result{}, err
			}

			//创建成功，更新状态
			if err = updateStatus(ctx, r, instance); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		} else {
			return ctrl.Result{}, err
		}

	}

	//根据单qps和总qps计算期望的副本数
	expectedReplicas := getExpectReplicas(instance)

	realReplicas := *deployment.Spec.Replicas

	logger.Sugar().Infof("expectReplicas [%d], realReplicas [%d]\n", expectedReplicas, realReplicas)

	if expectedReplicas == realReplicas {
		return ctrl.Result{}, nil
	}

	*(deployment.Spec.Replicas) = expectedReplicas

	if err = r.Update(ctx, deployment); err != nil {
		return ctrl.Result{}, err
	}

	if err = updateStatus(ctx, r, instance); err != nil {
		logger.Sugar().Errorf("update status error")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func getExpectReplicas(elasticweb *elasticwebdctcomv1.ElasticWeb) int32 {
	singlePodQps := *(elasticweb.Spec.SinglePodQPS)
	totalQps := *(elasticweb.Spec.TotalQPS)
	replicas := totalQps / singlePodQps

	if totalQps%singlePodQps > 0 {
		replicas++
	}

	return replicas
}

func createServiceIfNotExist(ctx context.Context, r *ElasticWebReconciler,
	elasticWeb *elasticwebdctcomv1.ElasticWeb, req ctrl.Request) error {

	svc := &corev1.Service{}
	err := r.Get(ctx, req.NamespacedName, svc)
	//查询结果没有出错，证明svc正常，就不做任何事情
	if err == nil {
		logger.Sugar().Infof("svc exists")
		return nil
	}

	if !errors.IsNotFound(err) {
		logger.Sugar().Error("查询%s svc失败", req.NamespacedName)
		return err
	}

	svc = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elasticWeb.Name,
			Namespace: elasticWeb.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Port:     8080,
					NodePort: *elasticWeb.Spec.Port,
				},
			},
			Selector: map[string]string{
				"app": APP_NAME,
			},
			Type: corev1.ServiceTypeNodePort,
		},
	}

	//建立关联
	if err := controllerutil.SetControllerReference(elasticWeb, svc, r.Scheme); err != nil {
		logger.Sugar().Infof("SetControllerReference error")
		return err
	}

	//创建svc
	logger.Sugar().Infof("start create svc")
	if err := r.Create(ctx, svc); err != nil {
		logger.Sugar().Errorf("create svc fail %s\n", err.Error())
		return err
	}

	logger.Sugar().Infof("create svc success")

	return nil
}

func createDeployment(ctx context.Context, r *ElasticWebReconciler,
	elasticWeb *elasticwebdctcomv1.ElasticWeb) error {

	//计算期望的pod数量
	expectReplicas := getExpectReplicas(elasticWeb)

	logger.Sugar().Infof("expectedReplcias [%d]", expectReplicas)

	//实例化一个数据结构
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      elasticWeb.Name,
			Namespace: elasticWeb.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32Ptr(expectReplicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": APP_NAME,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": APP_NAME,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            APP_NAME,
							Image:           elasticWeb.Spec.Image,
							ImagePullPolicy: "IfNotPresent",
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: CONTAINER_PORT,
								},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"cpu":    resource.MustParse(CPU_LIMIT),
									"memory": resource.MustParse(MEM_LIMIT),
								},
								Requests: corev1.ResourceList{
									"cpu":    resource.MustParse(CPU_REQUEST),
									"memory": resource.MustParse(MEM_REQUEST),
								},
							},
						},
					},
				},
			},
		},
	}

	//建立关联，删除elasticweb资源时就会将deployment也删除掉
	logger.Sugar().Infof("set reference")
	if err := controllerutil.SetControllerReference(elasticWeb, deployment, r.Scheme); err != nil {
		logger.Sugar().Errorf("set controler reference error %s\n", err.Error())
		return err
	}

	// 创建deployment
	logger.Sugar().Infof("start create deployment")
	if err := r.Create(ctx, deployment); err != nil {
		logger.Sugar().Errorf("create deployment error %s\n", err.Error())
		return err
	}

	logger.Sugar().Infof("create deployment success")

	return nil
}

func updateStatus(ctx context.Context, r *ElasticWebReconciler,
	elasticWeb *elasticwebdctcomv1.ElasticWeb) error {

	//单个pod的qps
	singlePodQPS := *(elasticWeb.Spec.SinglePodQPS)

	//pod总数
	replicas := getExpectReplicas(elasticWeb)

	if elasticWeb.Status.RealQPS == nil {
		elasticWeb.Status.RealQPS = new(int32)
	}

	*(elasticWeb.Status.RealQPS) = singlePodQPS * replicas

	logger.Sugar().Infof("singlePodQPS [%d], replicas [%d], realQPS[%d]", singlePodQPS, replicas, *(elasticWeb.Status.RealQPS))

	if err := r.Update(ctx, elasticWeb); err != nil {
		logger.Sugar().Errorf("update status err %s\n", err.Error())
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ElasticWebReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&elasticwebdctcomv1.ElasticWeb{}).
		Complete(r)
}
