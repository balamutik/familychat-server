#!/usr/bin/env python3
"""TURN Allocate probe using the same temporary REST credentials as GET /calls/{id}/ice."""
import base64
import hashlib
import hmac
import pathlib
import secrets
import socket
import struct
import time

MAGIC=0x2112A442

def attribute(kind,value):
    return struct.pack('!HH',kind,len(value))+value+b'\0'*((-len(value))%4)

def packet(attrs,txid,integrity=None):
    length=len(attrs)+(24 if integrity else 0)
    header=struct.pack('!HHI',3,length,MAGIC)+txid
    if integrity:
        return header+attrs+attribute(8,hmac.new(integrity,header+attrs,hashlib.sha1).digest())
    return header+attrs

def attributes(data):
    result={}; off=20
    while off+4<=len(data):
        kind,length=struct.unpack('!HH',data[off:off+4]); off+=4
        result[kind]=data[off:off+length]; off+=(length+3)&~3
    return result

def send(sock,data):
    sock.sendto(data,('127.0.0.1',3478))
    response,_=sock.recvfrom(2048)
    return struct.unpack('!H',response[:2])[0],attributes(response)

def code(attrs):
    value=attrs.get(9,b'\0\0\0\0')
    return value[2]*100+value[3]

def main():
    env=dict(line.split('=',1) for line in pathlib.Path('.env').read_text().splitlines() if '=' in line)
    username=str(int(time.time())+600)+':probe'
    password=base64.b64encode(hmac.new(env['TURN_SECRET'].encode(),username.encode(),hashlib.sha1).digest()).decode()
    with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as sock:
        sock.settimeout(3)
        requested=attribute(0x0019,b'\x11\0\0\0')
        status,challenge=send(sock,packet(requested,secrets.token_bytes(12)))
        if status!=0x0113 or code(challenge)!=401:
            raise AssertionError(f'expected TURN challenge, got type={status:x} code={code(challenge)}')
        realm,nonce=challenge[0x0014],challenge[0x0015]
        base=requested+attribute(0x0006,username.encode())+attribute(0x0014,realm)+attribute(0x0015,nonce)
        def attempt(value):
            key=hashlib.md5(username.encode()+b':'+realm+b':'+value.encode()).digest()
            return send(sock,packet(base,secrets.token_bytes(12),key))
        status,attrs=attempt(password)
        if status!=0x0103 or 0x0016 not in attrs:
            raise AssertionError(f'valid TURN allocation failed: type={status:x} code={code(attrs)}')
        # A second UDP socket avoids the allocation-mismatch response for an existing 5-tuple.
    with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as sock:
        sock.settimeout(3)
        _,new_challenge=send(sock,packet(requested,secrets.token_bytes(12)))
        new_realm,new_nonce=new_challenge[0x0014],new_challenge[0x0015]
        wrong_base=requested+attribute(0x0006,username.encode())+attribute(0x0014,new_realm)+attribute(0x0015,new_nonce)
        wrong_key=hashlib.md5(username.encode()+b':'+new_realm+b':wrong').digest()
        status,attrs=send(sock,packet(wrong_base,secrets.token_bytes(12),wrong_key))
        if status!=0x0113 or code(attrs)!=401:
            raise AssertionError(f'invalid TURN credential accepted: type={status:x} code={code(attrs)}')
    print('PASS: temporary TURN credential allocates; incorrect credential rejected')

if __name__=='__main__':
    main()
