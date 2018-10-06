package main

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	watch2 "k8s.io/apimachinery/pkg/watch"
	"os"

	"path/filepath"
	"net/http"
	"fmt"
	"github.com/nlopes/slack"
	"strings"
	"log"
	"bytes"
	"io/ioutil"
	"encoding/json"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v12 "k8s.io/api/rbac/v1"
)

var clientset *kubernetes.Clientset
var host string
var token = ""

func main() {
	token = os.Getenv("SLACK_TOKEN")
	configFile := filepath.Join(homedir.HomeDir(), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", configFile)
	host = config.Host
	if err != nil {
		panic(err.Error())
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	http.HandleFunc("/", handler)
	http.ListenAndServe(":8080", nil)

}

type K8sRequest struct {
	UserName    string
	UserID      string
	ResponseUrl string
	K8s         *kubernetes.Clientset
	Host        string
}

func NewK8sRequest(slash slack.SlashCommand, k8s *kubernetes.Clientset, host string) K8sRequest {
	return K8sRequest{
		UserName:    slash.UserName,
		UserID:      slash.UserID,
		ResponseUrl: slash.ResponseURL,
		K8s:         k8s,
		Host:        host,
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	slash, err := slack.SlashCommandParse(r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("unable to parse slash command: ", err.Error())
		return
	}
	if !strings.HasPrefix(slash.ResponseURL, "https://hooks.slack.com/") {
		w.WriteHeader(http.StatusBadRequest)
		log.Println("invalid response url", slash.ResponseURL)
		return
	}
	if slash.Token != token {
		w.WriteHeader(http.StatusUnauthorized)
		log.Println("invalid token url", slash)
		return
	}
	req := NewK8sRequest(slash, clientset, host)

	w.WriteHeader(http.StatusOK)

	go func() {
		req.sendSimpleResponse(fmt.Sprintf("configuring namespace [%s], hang tight", req.NsName()))
		req.deleteUserNs()
		err = req.createNamespace()
		if err != nil {
			log.Printf("failed to create namespace [%s]: %+v", req.NsName(), err)
			req.sendSimpleResponse(fmt.Sprintf("failed to configure namespace %s", err.Error()))
			return
		}
		err = req.configureResourceLimits()
		if err != nil {
			println(err.Error())
			req.sendSimpleResponse(fmt.Sprintf("failed to configure your namespace: %s", err.Error()))
			return
		}
		sa, err := req.createServiceAccount()
		if err != nil {
			println(err.Error())
			req.sendSimpleResponse(fmt.Sprintf("failed to configure your namespace: %s", err.Error()))
			return
		}
		secretName, err := req.getSecretName(sa)
		if err != nil {
			println(err.Error())
			req.sendSimpleResponse(fmt.Sprintf("failed to get account secret: %s", err.Error()))
			return
		}
		secretValue, err := req.getSecretValue(secretName)
		if err != nil {
			println(err.Error())
			req.sendSimpleResponse(fmt.Sprintf("failed to get account secret: %s", err.Error()))
		}
		err = req.bindRole(sa.Name)
		if err != nil {
			println(err.Error())
			req.sendSimpleResponse(fmt.Sprintf("failed to assign permissions: %s", err.Error()))
		}
		req.sendResponse(&slack.Msg{
			Text:         ":tada: namespace configured! :tada:\n go <https://kubernetes.io/docs/tasks/tools/install-kubectl/#install-kubectl|install kubectl> then execute the following commands:",
			ResponseType: "in_channel",
			Attachments: []slack.Attachment{
				slack.Attachment{
					Pretext: "create a kube config entry for the cluster",
					Text:    fmt.Sprintf("`kubectl config set-cluster i0-k8s-workshop-cluster --server=%s --insecure-skip-tls-verify=true`", req.Host),
				},
				slack.Attachment{
					Pretext: "create a kube config entry for your account",
					Text:    fmt.Sprintf("`kubectl config set-credentials i0-k8s-workshop-user --token=%s`", secretValue),
				},

				slack.Attachment{
					Pretext: "create a kube config context that links your account and the cluster",
					Text:    fmt.Sprintf("`kubectl config set-context i0-k8s-workshop --cluster=i0-k8s-workshop-cluster --user=i0-k8s-workshop-user --namespace=%s`", req.NsName()),
				},
				slack.Attachment{
					Pretext: "tell kubectl to use the context by default",
					Text:    "`kubectl config use-context i0-k8s-workshop`",
				},
			},
		})

	}()

}

func (r *K8sRequest) sendSimpleResponse(message string) {
	payload := &slack.Msg{
		Text:         message,
		ResponseType: "in_channel",
	}
	r.sendResponse(payload)
}
func (r *K8sRequest) sendResponse(message *slack.Msg) {
	b, err := json.Marshal(message)
	if err != nil {
		log.Printf("failed to serialize message %+v", message)
		return
	}
	resp, err := http.Post(r.ResponseUrl, "application/json", bytes.NewReader(b))
	if err != nil {
		log.Printf("faild to send slack message: [%s]", err.Error())
		return
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Printf("failed to send message, can't read response from slack: [%s], original message: %+v", err.Error(), message)
			return
		}
		log.Printf("failed to send slack message: [%s], original message: %+v", string(body), message)
	}
}

func (r K8sRequest) NsName() string {
	sanitized := strings.Replace(r.UserName, ".", "-", -1)
	return sanitized + "-workspace"
}

func (r K8sRequest) deleteUserNs() {
	nsl, err := r.K8s.CoreV1().Namespaces().List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("userId = %s", r.UserID),
	})
	if err != nil {
		log.Printf("failed to list namespaces for user %s: %s", r.UserID, err.Error())
		return
	}
	for _, ns := range nsl.Items {
		gp := int64(0)
		pp := metav1.DeletePropagationForeground
		err = r.K8s.CoreV1().Namespaces().Delete(ns.Name, &metav1.DeleteOptions{
			GracePeriodSeconds: &gp,
			PropagationPolicy:  &pp,
		})
		if err != nil {
			log.Printf("failed to delete ns %s: %s", ns.Name, err.Error())
		} else {
			log.Printf("deleted ns %s", ns.Name)
		}
	}
}

func (r K8sRequest) createNamespace() error {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.NsName(),
			Labels: map[string]string{
				"userId": r.UserID,
			},
		},
	}

	_, err := r.K8s.CoreV1().Namespaces().Create(ns)
	return err
}

func (r K8sRequest) configureResourceLimits() error {
	quota := &v1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resource-quotas",
		},
		Spec: v1.ResourceQuotaSpec{
			Hard: v1.ResourceList{
				"pods": resource.MustParse("10"),
			},
		},
	}
	_, err := r.K8s.CoreV1().ResourceQuotas(r.NsName()).Create(quota)
	return err
}

func (r K8sRequest) createServiceAccount() (*v1.ServiceAccount, error) {
	sa := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.UserName,
			Labels: map[string]string{
				"userId": r.UserID,
			},
		},
	}
	return r.K8s.CoreV1().ServiceAccounts(r.NsName()).Create(sa)
}

func (r K8sRequest) getSecretName(account *v1.ServiceAccount) (string, error) {
	opts := metav1.ListOptions{
		Watch:         true,
		LabelSelector: "userId = " + r.UserID,
	}
	watch, err := r.K8s.CoreV1().ServiceAccounts(r.NsName()).Watch(opts)
	if err != nil {
		return "", err
	}
	for {
		x := <-watch.ResultChan()
		if x.Type == watch2.Added {
			acct := x.Object.(*v1.ServiceAccount)
			return acct.Secrets[0].Name, nil
		}
	}
}

func (r K8sRequest) getSecretValue(sn string) (string, error) {
	println("getting secret", sn)
	secret, err := r.K8s.CoreV1().Secrets(r.NsName()).Get(sn, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(secret.Data["token"]), nil
}

func (r K8sRequest) bindRole(sa string) error {
	rb := &v12.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: sa + "-edit-binding",
		},
		Subjects: []v12.Subject{
			v12.Subject{
				Kind:      "ServiceAccount",
				Name:      sa,
				Namespace: r.NsName(),
			},
		},
		RoleRef: v12.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "edit",
		},
	}
	_, err := r.K8s.RbacV1().RoleBindings(r.NsName()).Create(rb)
	return err
}
