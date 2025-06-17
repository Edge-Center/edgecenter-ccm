# EC-CCM Helm Chart

Helm chart для установки **Edgecenter Cloud Controller Manager (CCM)** в Kubernetes кластер.

## Описание

Этот Helm chart устанавливает компонент Cloud Controller Manager от Edgecenter, предназначенный для интеграции кластера Kubernetes с облачной инфраструктурой Edgecenter.

## Установка

Перед установкой убедитесь, что у вас есть доступ к Kubernetes кластеру и установлен Helm.

### 1. Установите чарт

```sh
helm install ec-ccm ./ec-ccm -n kube-system
```

Или с кастомными значениями:

```sh
helm install ec-ccm ./ec-ccm -n kube-system -f values.yaml
```

## Параметры

Параметры, которые можно настроить в `values.yaml`:

| Параметр                       | Тип     | Описание                                         | Обязателен | Значение по умолчанию         |
|--------------------------------|---------|--------------------------------------------------|------------|--------------------------------|
| `namespace`                    | string  | Целевое пространство имён Kubernetes             | Нет        | `kube-system`                  |
| `image.repository`             | string  | Образ контейнера CCM                             | Да         | `ghcr.io/edge-center/edgecenter-ccm` |
| `image.tag`                    | string  | Тег образа                                       | Нет        | `latest`                       |
| `image.pullPolicy`            | string  | Политика извлечения образа                       | Нет        | `IfNotPresent`                 |
| `cloudConfig.networkId`        | string  | ID сети Edgecenter                               | Да         | `"<network_id>"`               |
| `cloudConfig.subnetId`         | string  | ID подсети                                       | Да         | `"<subnet_id>"`                |
| `cloudConfig.floatingNetworkId`| string  | ID публичной сети                                | Да         | `"<floating_network_id>"`     |
| `cloudConfig.apiUrl`           | string  | API URL облака                                   | Да         | `"<api_url>"`                  |
| `cloudConfig.projectId`        | string  | ID проекта в Edgecenter                          | Да         | `"<project_id>"`               |
| `cloudConfig.regionId`         | string  | ID региона                                       | Да         | `"<region_id>"`                |
| `cloudConfig.apiKey`           | string  | API ключ для доступа                             | Да         | `"<api_key>"`                  |
| `cloudConfig.clusterId`        | string  | ID кластера                                      | Да         | `"<cluster_id>"`              |

## Обновление

Для обновления чарта используйте:

```sh
helm upgrade ec-ccm ./ec-ccm -n kube-system
```

## Удаление

Для удаления:

```sh
helm uninstall ec-ccm -n kube-system
```

## Контакты

Поддержка и документация: [https://edgecenter.ru](https://edgecenter.ru)

