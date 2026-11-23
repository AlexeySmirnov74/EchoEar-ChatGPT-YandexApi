# Experimental image-search result contract

The Unix-socket `/search` endpoint retains its existing request, isolation,
address validation, download/decoder limits and no-request-logging behavior.
Responses are:

* `200 application/octet-stream`: exactly 115200 bytes of RGB565 frame data.
* `204 No Content`: Yandex explicitly reported no search results, or its XML
  contained a recognized empty result structure; the response body is empty.
* `502`: all other upstream, malformed response, unsafe URL, network, download
  or decoder failures. An empty list of *usable* URLs is not evidence that no
  images exist.

Existing clients that only support 200 treat 204 as a generic failure. The
HTTP framing between the search worker and its parent uses a one-byte code for
the empty result and never contains the search query or a URL. Fixed errors do
not forward the upstream XML error message.

Yandex documents XML error 15 as “no search results”:
https://aistudio.yandex.ru/ru/docs/search-api/reference/error-codes

This directory is a separate lab snapshot; production was not changed.
