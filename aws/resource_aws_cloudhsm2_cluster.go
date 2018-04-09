package aws

import (
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/terraform/helper/schema"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudhsmv2"
	"github.com/hashicorp/terraform/helper/resource"
)

func resourceAwsCloudHsm2Cluster() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsCloudHsm2ClusterCreate,
		Read:   resourceAwsCloudHsm2ClusterRead,
		Update: resourceAwsCloudHsm2ClusterUpdate,
		Delete: resourceAwsCloudHsm2ClusterDelete,
		Importer: &schema.ResourceImporter{
			State: resourceAwsCloudHsm2ClusterImport,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(120 * time.Minute),
			Update: schema.DefaultTimeout(120 * time.Minute),
			Delete: schema.DefaultTimeout(120 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"backup_identifier": {
				Type:     schema.TypeString,
				Computed: false,
				Optional: true,
				ForceNew: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},

			"hsm_type": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validateCloudHsm2HsmType,
			},

			"subnet_ids": {
				Type:     schema.TypeSet,
				Required: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
				Set:      schema.HashString,
				ForceNew: true,
			},

			"cluster_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"vpc_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"cluster_certificates": {
				Type:     schema.TypeSet,
				Computed: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"cluster_certificate": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"cluster_csr": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"aws_hardware_certificate": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"hsm_certificate": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"manufacturer_hardware_certificate": {
							Type:     schema.TypeString,
							Computed: true,
						},
					},
				},
			},

			"security_group_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"cluster_state": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"tags": tagsSchema(),
		},
	}
}

func resourceAwsCloudHsm2ClusterImport(
	d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	d.Set("cluster_id", d.Id())
	return []*schema.ResourceData{d}, nil
}

func describeCloudHsm2Cluster(clusterId string, meta interface{}) (*cloudhsmv2.Cluster, error) {
	conn := meta.(*AWSClient).cloudhsmv2conn

	filters := []*string{&clusterId}
	result := int64(1)
	out, err := conn.DescribeClusters(&cloudhsmv2.DescribeClustersInput{
		Filters: map[string][]*string{
			"clusterIds": filters,
		},
		MaxResults: &result,
	})
	if err != nil {
		log.Printf("[WARN] Error on retrieving CloudHSMv2 Cluster (%s) when waiting: %s", clusterId, err)
		return nil, err
	}

	var cluster *cloudhsmv2.Cluster

	for _, c := range out.Clusters {
		if *c.ClusterId == clusterId {
			cluster = c
		}
	}
	return cluster, nil
}

func resourceAwsCloudHsm2ClusterRefreshFunc(
	d *schema.ResourceData, meta interface{}) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		cluster, err := describeCloudHsm2Cluster(d.Id(), meta)

		if cluster == nil {
			return 42, "destroyed", nil
		}

		if cluster.State != nil {
			log.Printf("[DEBUG] CloudHSMv2 Cluster status (%s): %s", d.Id(), *cluster.State)
		}

		return cluster, *cluster.State, err
	}
}

func resourceAwsCloudHsm2ClusterCreate(d *schema.ResourceData, meta interface{}) error {
	cloudhsm2 := meta.(*AWSClient).cloudhsmv2conn

	subnetIdsSet := d.Get("subnet_ids").(*schema.Set)
	subnetIds := make([]*string, subnetIdsSet.Len())
	for i, subnetId := range subnetIdsSet.List() {
		subnetIds[i] = aws.String(subnetId.(string))
	}

	input := &cloudhsmv2.CreateClusterInput{
		HsmType:   aws.String(d.Get("hsm_type").(string)),
		SubnetIds: subnetIds,
	}

	backupId := d.Get("backup_identifier").(string)
	if len(backupId) != 0 {
		input.SourceBackupId = aws.String(backupId)
	}

	log.Printf("[DEBUG] CloudHSMv2 Cluster create %s", input)

	var output *cloudhsmv2.CreateClusterOutput

	err := resource.Retry(180*time.Second, func() *resource.RetryError {
		var err error
		output, err = cloudhsm2.CreateCluster(input)
		if err != nil {
			if isAWSErr(err, cloudhsmv2.ErrCodeCloudHsmInternalFailureException, "request was rejected because of an AWS CloudHSM internal failure") {
				log.Printf("[DEBUG] CloudHSMv2 Cluster re-try creating %s", input)
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(err)
		}
		return nil
	})

	if err != nil {
		return err
	}

	d.SetId(aws.StringValue(output.Cluster.ClusterId))
	log.Printf("[INFO] CloudHSMv2 Cluster ID: %s", d.Id())
	log.Println("[INFO] Waiting for CloudHSMv2 Cluster to be available")

	targetState := cloudhsmv2.ClusterStateUninitialized
	if len(backupId) > 0 {
		targetState = cloudhsmv2.ClusterStateActive
	}

	stateConf := &resource.StateChangeConf{
		Pending:    []string{cloudhsmv2.ClusterStateCreateInProgress, cloudhsmv2.ClusterStateInitializeInProgress},
		Target:     []string{targetState},
		Refresh:    resourceAwsCloudHsm2ClusterRefreshFunc(d, meta),
		Timeout:    d.Timeout(schema.TimeoutCreate),
		MinTimeout: 30 * time.Second,
		Delay:      30 * time.Second,
	}

	// Wait, catching any errors
	_, errWait := stateConf.WaitForState()
	if errWait != nil {
		if len(backupId) == 0 {
			return fmt.Errorf("[WARN] Error waiting for CloudHSMv2 Cluster state to be \"UNINITIALIZED\": %s", errWait)
		} else {
			return fmt.Errorf("[WARN] Error waiting for CloudHSMv2 Cluster state to be \"ACTIVE\": %s", errWait)
		}
	}

	if err := setTagsAwsCloudHsm2Cluster(cloudhsm2, d); err != nil {
		return err
	}

	return resourceAwsCloudHsm2ClusterRead(d, meta)
}

func resourceAwsCloudHsm2ClusterRead(d *schema.ResourceData, meta interface{}) error {

	cluster, err := describeCloudHsm2Cluster(d.Id(), meta)

	if cluster == nil {
		log.Printf("[WARN] CloudHSMv2 Cluster (%s) not found", d.Id())
		d.SetId("")
		return err
	}

	log.Printf("[INFO] Reading CloudHSMv2 Cluster Information: %s", d.Id())

	d.Set("cluster_id", cluster.ClusterId)
	d.Set("cluster_state", cluster.State)
	d.Set("security_group_id", cluster.SecurityGroup)
	d.Set("vpc_id", cluster.VpcId)
	d.Set("backup_identifier", cluster.SourceBackupId)
	d.Set("hsm_type", cluster.HsmType)
	d.Set("cluster_certificate", readCloudHsm2ClusterCertificates(cluster))

	var subnets []string
	for _, sn := range cluster.SubnetMapping {
		subnets = append(subnets, *sn)
	}
	if err := d.Set("subnet_ids", subnets); err != nil {
		return fmt.Errorf("[DEBUG] Error saving Subnet IDs to state for CloudHSMv2 Cluster (%s): %s", d.Id(), err)
	}

	return nil
}

func resourceAwsCloudHsm2ClusterUpdate(d *schema.ResourceData, meta interface{}) error {
	cloudhsm2 := meta.(*AWSClient).cloudhsmv2conn

	if err := setTagsAwsCloudHsm2Cluster(cloudhsm2, d); err != nil {
		return err
	}

	return resourceAwsCloudHsm2ClusterRead(d, meta)
}

func resourceAwsCloudHsm2ClusterDelete(d *schema.ResourceData, meta interface{}) error {
	cloudhsm2 := meta.(*AWSClient).cloudhsmv2conn

	var output *cloudhsmv2.DeleteClusterOutput
	log.Printf("[DEBUG] CloudHSMv2 Delete cluster: %s", d.Id())
	err := resource.Retry(180*time.Second, func() *resource.RetryError {
		var err error
		output, err = cloudhsm2.DeleteCluster(&cloudhsmv2.DeleteClusterInput{
			ClusterId: aws.String(d.Id()),
		})
		if err != nil {
			if isAWSErr(err, cloudhsmv2.ErrCodeCloudHsmInternalFailureException, "request was rejected because of an AWS CloudHSM internal failure") {
				log.Printf("[DEBUG] CloudHSMv2 Cluster re-try deleting %s", d.Id())
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(err)
		}
		return nil
	})

	if err != nil {
		return err
	}
	log.Println("[INFO] Waiting for CloudHSMv2 Cluster to be deleted")

	stateConf := &resource.StateChangeConf{
		Pending:    []string{cloudhsmv2.ClusterStateDeleteInProgress},
		Target:     []string{cloudhsmv2.ClusterStateDeleted},
		Refresh:    resourceAwsCloudHsm2ClusterRefreshFunc(d, meta),
		Timeout:    d.Timeout(schema.TimeoutCreate),
		MinTimeout: 30 * time.Second,
		Delay:      30 * time.Second,
	}

	// Wait, catching any errors
	_, errWait := stateConf.WaitForState()
	if errWait != nil {
		return fmt.Errorf("[WARN] Error waiting for CloudHSMv2 Cluster state to be \"DELETED\": %s", errWait)
	}

	return nil
}

func setTagsAwsCloudHsm2Cluster(conn *cloudhsmv2.CloudHSMV2, d *schema.ResourceData) error {
	if d.HasChange("tags") {
		oraw, nraw := d.GetChange("tags")
		create, remove := diffTagsGeneric(oraw.(map[string]interface{}), nraw.(map[string]interface{}))

		if len(remove) > 0 {
			log.Printf("[DEBUG] Removing tags: %#v", remove)
			keys := make([]*string, 0, len(remove))
			for k, _ := range remove {
				keys = append(keys, aws.String(k))
			}

			_, err := conn.UntagResource(&cloudhsmv2.UntagResourceInput{
				ResourceId: aws.String(d.Id()),
				TagKeyList: keys,
			})
			if err != nil {
				return err
			}
		}
		if len(create) > 0 {
			log.Printf("[DEBUG] Creating tags: %#v", create)
			tagList := make([]*cloudhsmv2.Tag, 0, len(create))
			for k, v := range create {
				tagList = append(tagList, &cloudhsmv2.Tag{
					Key:   &k,
					Value: v,
				})
			}
			_, err := conn.TagResource(&cloudhsmv2.TagResourceInput{
				ResourceId: aws.String(d.Id()),
				TagList:    tagList,
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func validateCloudHsm2HsmType(v interface{}, k string) (ws []string, errors []error) {
	value := v.(string)
	hsmType := "hsm1.medium"
	if value != hsmType {
		errors = append(errors, fmt.Errorf("there is only %s HSM type available", hsmType))
	}
	return
}

func readCloudHsm2ClusterCertificates(cluster *cloudhsmv2.Cluster) []map[string]interface{} {
	certs := map[string]interface{}{}
	if cluster.Certificates != nil {
		if aws.StringValue(cluster.State) == "UNINITIALIZED" {
			certs["cluster_csr"] = aws.StringValue(cluster.Certificates.ClusterCsr)
			certs["aws_hardware_certificate"] = aws.StringValue(cluster.Certificates.AwsHardwareCertificate)
			certs["hsm_certificate"] = aws.StringValue(cluster.Certificates.HsmCertificate)
			certs["manufacturer_hardware_certificate"] = aws.StringValue(cluster.Certificates.ManufacturerHardwareCertificate)
		} else if aws.StringValue(cluster.State) == "ACTIVE" {
			certs["cluster_certificate"] = aws.StringValue(cluster.Certificates.ClusterCertificate)
		}
	}
	return []map[string]interface{}{certs}
}
