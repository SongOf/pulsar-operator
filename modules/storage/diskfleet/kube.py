"""Small Kubernetes REST client; service-account credentials are rotated per request."""
import json
import os
import ssl
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import Request, urlopen


class KubeError(RuntimeError):
    def __init__(self, status, message):
        self.status = status
        super().__init__(message)


class KubeClient:
    def __init__(self, url=None, token_file=None, ca_file=None):
        sa = Path('/var/run/secrets/kubernetes.io/serviceaccount')
        host = os.environ.get('KUBERNETES_SERVICE_HOST', 'kubernetes.default.svc')
        port = os.environ.get('KUBERNETES_SERVICE_PORT_HTTPS', '443')
        self.url = (url or os.environ.get('KUBE_API_URL') or f'https://{host}:{port}').rstrip('/')
        self.token_file = Path(token_file or os.environ.get('KUBE_TOKEN_FILE', str(sa / 'token')))
        ca = ca_file or os.environ.get('KUBE_CA_FILE') or (str(sa / 'ca.crt') if (sa / 'ca.crt').exists() else None)
        parsed = urlsplit(self.url)
        if parsed.scheme != 'https' and not (parsed.scheme == 'http' and parsed.hostname in ('127.0.0.1', 'localhost', '::1')):
            raise ValueError('Kubernetes API must use HTTPS (HTTP allowed only for a local kubectl proxy)')
        self.context = ssl.create_default_context(cafile=ca)

    def request(self, method, path, body=None):
        headers = {'Accept': 'application/json'}
        if self.token_file.exists():
            headers['Authorization'] = 'Bearer ' + self.token_file.read_text().strip()
        if body is not None:
            headers['Content-Type'] = 'application/merge-patch+json' if method == 'PATCH' else 'application/json'
        req = Request(self.url + path, data=json.dumps(body).encode() if body is not None else None, headers=headers, method=method)
        try:
            with urlopen(req, context=self.context, timeout=20) as response:
                data = response.read()
                return json.loads(data) if data else {}
        except HTTPError as error:
            try:
                message = json.loads(error.read()).get('message', str(error))
            except (ValueError, UnicodeError):
                message = str(error)
            raise KubeError(error.code, message) from error
        except (URLError, TimeoutError) as error:
            raise KubeError(503, 'Kubernetes API unavailable: ' + str(error)) from error

    def list(self, path, label_selector=None, field_selector=None):
        params = {'limit': 500}
        if label_selector:
            params['labelSelector'] = label_selector
        if field_selector:
            params['fieldSelector'] = field_selector
        items = []
        while True:
            page = self.request('GET', path + '?' + urlencode(params))
            items.extend(page.get('items', []))
            cursor = page.get('metadata', {}).get('continue')
            if not cursor:
                return items
            params['continue'] = cursor
