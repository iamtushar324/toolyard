#!/usr/bin/env python3
"""Create the dedicated Swarm proxy. Never changes the live Toolyard router."""
import subprocess,socket
host='toolyard.stage.dev.beknown.live'
labels={
 'traefik.enable':'true',
 'traefik.http.middlewares.toolyard-stage-https.redirectscheme.scheme':'https',
 'traefik.http.middlewares.toolyard-stage-https.redirectscheme.permanent':'true',
 'traefik.http.routers.toolyard-stage.entrypoints':'web',
 'traefik.http.routers.toolyard-stage.rule':f'Host(`{host}`)',
 'traefik.http.routers.toolyard-stage.middlewares':'toolyard-stage-https',
 'traefik.http.routers.toolyard-stage-secure.entrypoints':'websecure',
 'traefik.http.routers.toolyard-stage-secure.rule':f'Host(`{host}`)',
 'traefik.http.routers.toolyard-stage-secure.tls.certresolver':'le',
 'traefik.http.services.toolyard-stage.loadbalancer.server.port':'18792',
}
args=['docker','service','create','--name','toolyard-stage-proxy','--network','bk-dev','--constraint','node.hostname=='+socket.gethostname(),'--limit-memory','48M','--limit-cpu','0.25']
for k,v in labels.items(): args+=['--label',f'{k}={v}']
args+=['alpine/socat:latest@sha256:d85531a29ef5ba99dfb4717485c239307e2902d522a1bc010992a2728c92cfad','tcp-listen:18792,fork,reuseaddr','tcp:172.23.0.1:18792']
subprocess.run(args,check=True)
