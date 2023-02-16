
- Try out istio gateway locally - see what IP it gets
- annotation on gateway to say what the default subdomain is (from root MZ e.g. tenant1.managed.zone)
    - Means we don't need to lookup the MZ
    - https://github.com/Kuadrant/multi-cluster-traffic-controller/issues/66
- Is host a customer defined zone or a default zone
    - Does it simplify things 
    - Create a MZ in the tenant for a different zone? Use a subdomain of that zone in the gateway instead of the default zone (Show DNS delegation!!!)
- DNSRecords need to be in the same ns as the MZ
    - need to lookup the default MZ and use its namespace for DNSRecord
- Step 1 as above (show default domain/mz )
- Step 2 is to create a MZ for my own domain (show dns delegation)
- (Possible Step 3) Add a listener for a custom host
    - Open Q's about CNAME and host generation

- Check if HTTPRoute assignement/creation/linkning is in the gateway status (attached?)
- Namespace selector in Gateway listeners

- Transforms pushed to M3