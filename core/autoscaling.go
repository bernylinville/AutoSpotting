// Copyright (c) 2016-2019 Cristian Măgherușan-Stanciu
// Licensed under the Open Software License version 3.0

package autospotting

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
)

type autoScalingGroup struct {
	*autoscaling.Group

	name                string
	region              *region
	launchConfiguration *launchConfiguration
	launchTemplate      *launchTemplate
	instances           instances
	config              AutoScalingConfig
}

func (a *autoScalingGroup) loadLaunchConfiguration() (*launchConfiguration, error) {
	//already done
	if a.launchConfiguration != nil {
		return a.launchConfiguration, nil
	}

	lcName := a.LaunchConfigurationName

	if lcName == nil {
		return nil, errors.New("missing launch configuration")
	}

	svc := a.region.services.autoScaling

	params := &autoscaling.DescribeLaunchConfigurationsInput{
		LaunchConfigurationNames: []*string{lcName},
	}
	resp, err := svc.DescribeLaunchConfigurations(params)

	if err != nil {
		log.Println(err.Error())
		return nil, err
	}

	a.launchConfiguration = &launchConfiguration{
		LaunchConfiguration: resp.LaunchConfigurations[0],
	}
	return a.launchConfiguration, nil
}

func (a *autoScalingGroup) loadLaunchTemplate() (*launchTemplate, error) {
	//already done
	if a.launchTemplate != nil {
		return a.launchTemplate, nil
	}

	lt := a.LaunchTemplate

	if lt == nil {
		return nil, errors.New("missing launch template")
	}

	ltID := lt.LaunchTemplateId
	ltVer := lt.Version

	if ltID == nil || ltVer == nil {
		return nil, errors.New("missing launch template")
	}

	svc := a.region.services.ec2

	params := &ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: ltID,
		Versions:         []*string{ltVer},
	}

	resp, err := svc.DescribeLaunchTemplateVersions(params)

	if err != nil {
		log.Println(err.Error())
		return nil, err
	}

	if len(resp.LaunchTemplateVersions) == 0 {
		return nil, errors.New("missing launch template")
	}

	ltv := resp.LaunchTemplateVersions[0]

	params2 := &ec2.DescribeImagesInput{
		ImageIds: []*string{ltv.LaunchTemplateData.ImageId},
	}

	resp2, err2 := svc.DescribeImages(params2)

	if err2 != nil {
		log.Println(err2.Error())
		return nil, err2
	}

	if len(resp2.Images) == 0 {
		return nil, errors.New("missing launch template image")
	}

	a.launchTemplate = &launchTemplate{
		LaunchTemplateVersion: ltv,
		Image:                 resp2.Images[0],
	}
	return a.launchTemplate, nil
}

func (a *autoScalingGroup) needReplaceOnDemandInstances() (bool, int64) {
	onDemandRunning, totalRunning := a.alreadyRunningInstanceCount(false, nil)
	debug.Printf("onDemandRunning=%v totalRunning=%v a.minOnDemand=%v",
		onDemandRunning, totalRunning, a.config.MinOnDemand)

	if totalRunning == 0 {
		log.Printf("The group %s is currently empty or in the process of launching new instances",
			a.name)
		return true, totalRunning
	}

	if onDemandRunning > a.config.MinOnDemand {
		log.Println("Currently more than enough OnDemand instances running")
		return true, totalRunning
	}

	if onDemandRunning == a.config.MinOnDemand {
		log.Println("Currently OnDemand running equals to the required number, skipping run")
		return false, totalRunning
	}
	log.Println("Currently fewer OnDemand instances than required !")
	return false, totalRunning
}

func (a *autoScalingGroup) terminateRandomSpotInstanceIfHavingEnough(totalRunning int64, wait bool) error {

	if totalRunning == 1 {
		log.Println("Warning: blocking replacement of very last instance - consider raising ASG to >= 2")
		return nil
	}

	if allInstancesAreRunning, onDemandRunning := a.allInstancesRunning(); allInstancesAreRunning {
		if a.instances.count64() == *a.DesiredCapacity && onDemandRunning == a.config.MinOnDemand {
			log.Println("Currently Spot running equals to the required number, skipping termination")
			return nil
		}

		if a.instances.count64() < *a.DesiredCapacity {
			log.Println("Not enough capacity in the group")
			return nil
		}
	}

	randomSpot := a.getAnySpotInstance()
	if randomSpot == nil {
		log.Println("Couldn't pick a random spot instance")
		return nil
	}

	log.Println("Terminating randomly-selected spot instance",
		*randomSpot.Instance.InstanceId)

	var isTerminated error
	switch a.config.TerminationMethod {
	case DetachTerminationMethod:
		isTerminated = randomSpot.terminate()
	default:
		isTerminated = a.terminateInstanceInAutoScalingGroup(randomSpot.Instance.InstanceId, wait, false)
	}

	if isTerminated == nil {
		// add to FinalRecap
		recapText := fmt.Sprintf("%s Terminated random spot instance %s [too few onDemands]", a.name, *randomSpot.Instance.InstanceId)
		a.region.conf.FinalRecap[a.region.name] = append(a.region.conf.FinalRecap[a.region.name], recapText)
	}

	return isTerminated
}

func (a *autoScalingGroup) allInstancesRunning() (bool, int64) {
	onDemandRunning, totalRunning := a.alreadyRunningInstanceCount(false, nil)
	return totalRunning == a.instances.count64(), onDemandRunning
}

func (a *autoScalingGroup) cronEventAction() runer {

	a.scanInstances()
	a.loadDefaultConfig()
	a.loadConfigFromTags()

	log.Println("Finding spot instances created for", a.name)

	spotInstance := a.findUnattachedInstanceLaunchedForThisASG()

	shouldRun := cronRunAction(time.Now(), a.config.CronSchedule, a.config.CronTimezone, a.config.CronScheduleState)
	debug.Println(a.region.name, a.name, "Should take replacement actions:", shouldRun)

	if !shouldRun {
		log.Println(a.region.name, a.name,
			"Skipping run, outside the enabled cron run schedule")
		return skipRun{reason: "outside-cron-schedule"}
	}

	if spotInstance == nil {
		log.Println("No spot instances were found for ", a.name)

		onDemandInstance := a.getAnyUnprotectedOnDemandInstance()

		if need, total := a.needReplaceOnDemandInstances(); !need {
			log.Printf("Not allowed to replace any more of the running OD instances in %s", a.name)
			return terminateSpotInstance{target{asg: a, totalInstances: total}}
		}

		if onDemandInstance == nil {
			log.Println(a.region.name, a.name,
				"No running unprotected on-demand instances were found, nothing to do here...")

			return skipRun{reason: "no-instances-to-replace"}
		}

		a.loadLaunchConfiguration()
		a.loadLaunchTemplate()

		if len(a.region.conf.SQSQueueURL) == 0 {
			return launchSpotReplacement{target{
				onDemandInstance: onDemandInstance}}
		}
		return sqsSendMessageOnInstanceLaunch{
			target{
				asg:              a,
				onDemandInstance: onDemandInstance,
			},
		}
	}

	spotInstanceID := *spotInstance.InstanceId
	log.Println("Found unattached spot instance", spotInstanceID)

	if need, total := a.needReplaceOnDemandInstances(); !need || !shouldRun {
		// add to FinalRecap
		recapText := fmt.Sprintf("%s Terminated spot instance %s [not needed]", a.name, spotInstanceID)
		a.region.conf.FinalRecap[a.region.name] = append(a.region.conf.FinalRecap[a.region.name], recapText)
		return terminateUnneededSpotInstance{
			target{
				asg:            a,
				spotInstance:   spotInstance,
				totalInstances: total,
			}}
	}

	if !spotInstance.isReadyToAttach(a) {
		log.Printf("Spot instance %s not yet ready, waiting for next run while processing %s",
			spotInstanceID,
			a.name)
		return skipRun{"spot instance replacement exists but not ready"}
	}

	log.Println(a.region.name, "Found spot instance:", spotInstanceID,
		"Attaching it to", a.name)

	return swapSpotInstance{target{
		asg:          a,
		spotInstance: spotInstance}}
}

func (a *autoScalingGroup) scanInstances() instances {

	log.Println("Adding instances to", a.name)
	a.instances = makeInstances()
	for _, inst := range a.Instances {
		i := a.region.instances.get(*inst.InstanceId)

		if i == nil {
			debug.Println("Missing instance data for ", *inst.InstanceId, "scanning it again")
			a.region.scanInstance(inst.InstanceId)

			i = a.region.instances.get(*inst.InstanceId)
			if i == nil {
				debug.Println("Failed to scan instance", *inst.InstanceId)
				continue
			}
		}

		i.asg, i.region = a, a.region
		if inst.ProtectedFromScaleIn != nil {
			i.protected = i.protected || *inst.ProtectedFromScaleIn
		}

		if i.isSpot() {
			i.price = i.typeInfo.pricing.spot[*i.Placement.AvailabilityZone]
		} else {
			i.price = i.typeInfo.pricing.onDemand + i.typeInfo.pricing.premium
		}

		// Avoid adding instance in Terminating (Wait|Proceed) Lifecycle State
		if strings.HasPrefix(*inst.LifecycleState, "Terminating") {
			continue
		}

		a.instances.add(i)
	}
	return a.instances
}

func (a *autoScalingGroup) replaceOnDemandInstanceWithSpot(spotInstanceID string) error {
	var odInstance *instance
	var err error

	// get the details of our spot instance so we can see its AZ
	log.Println(a.name, "Retrieving instance details for ", spotInstanceID)
	spotInst := a.region.instances.get(spotInstanceID)
	if spotInst == nil {
		return errors.New("couldn't find spot instance to use")
	}

	if len(a.region.conf.SQSQueueURL) == 0 {
		if odInstance, err = spotInst.swapWithGroupMember(a); err != nil {
			log.Printf("%s, couldn't perform spot replacement of %s ",
				a.region.name, *spotInst.InstanceId)
			return err
		}
		// add to FinalRecap
		recapText := fmt.Sprintf("%s OnDemand instance %s replaced with spot instance %s",
			a.name, *odInstance.InstanceId, *spotInst.InstanceId)
		a.region.conf.FinalRecap[a.region.name] = append(a.region.conf.FinalRecap[a.region.name], recapText)

	} else {

		if err := a.region.sqsSendMessageOnInstanceLaunch(&a.name, &spotInstanceID, spotInst.State.Name, "swap-with-on-demand"); err != nil {
			return err
		}
		// add to FinalRecap
		recapText := fmt.Sprintf("%s Sent spot instance %s event message to SQSQueue", a.name, *spotInst.InstanceId)
		a.region.conf.FinalRecap[a.region.name] = append(a.region.conf.FinalRecap[a.region.name], recapText)
	}
	return nil
}

// Returns the information about the first running instance found in
// the group, while iterating over all instances from the
// group. It can also filter by AZ and Lifecycle.
func (a *autoScalingGroup) getInstance(
	availabilityZone *string,
	onDemand bool,
	considerInstanceProtection bool,
) *instance {

	for i := range a.instances.instances() {

		// instance is running
		if *i.State.Name == ec2.InstanceStateNameRunning {

			// the InstanceLifecycle attribute is non-nil only for spot instances,
			// where it contains the value "spot", if we're looking for on-demand
			// instances only, then we have to skip the current instance.
			if (onDemand && i.isSpot()) || (!onDemand && !i.isSpot()) {
				debug.Println(a.name, "skipping instance", *i.InstanceId,
					"having different lifecycle than what we're looking for")
				continue
			}

			protT, err := i.isProtectedFromTermination()
			if err != nil {
				debug.Println(a.name, "failed to determine termination protection for", *i.InstanceId)
			}

			if considerInstanceProtection && (i.isProtectedFromScaleIn() || protT) {
				debug.Println(a.name, "skipping protected instance", *i.InstanceId)
				continue
			}

			if (availabilityZone != nil) && (*availabilityZone != *i.Placement.AvailabilityZone) {
				debug.Println(a.name, "skipping instance", *i.InstanceId,
					"placed in a different AZ than what we're looking for")
				continue
			}
			return i
		}
	}
	return nil
}

func (a *autoScalingGroup) getAnyUnprotectedOnDemandInstance() *instance {
	return a.getInstance(nil, true, true)
}

func (a *autoScalingGroup) getAnySpotInstance() *instance {
	return a.getInstance(nil, false, false)
}

func (a *autoScalingGroup) hasMemberInstance(inst *instance) bool {
	for _, member := range a.Instances {
		if *member.InstanceId == *inst.InstanceId {
			return true
		}
	}
	return false
}

func (a *autoScalingGroup) waitForInstanceStatus(instanceID *string, status string, maxRetry int) error {
	isInstanceInStatus := false
	for retry := 0; !isInstanceInStatus; retry++ {
		if retry > maxRetry {
			log.Printf("Failed waiting instance %s in status %s",
				*instanceID, status)
			break
		} else {
			result, err := a.region.services.autoScaling.DescribeAutoScalingInstances(
				&autoscaling.DescribeAutoScalingInstancesInput{
					InstanceIds: []*string{instanceID},
				})

			if err != nil {
				log.Println(err.Error())
				continue
			}

			autoScalingInstances := result.AutoScalingInstances

			if len(autoScalingInstances) > 0 {
				if instanceStatus := *autoScalingInstances[0].LifecycleState; instanceStatus != status {
					log.Printf("Waiting for instance %s to be in status %s [%s]",
						*instanceID, status, instanceStatus)
				} else {
					isInstanceInStatus = true
					return nil
				}
			} else {
				log.Printf("Waiting for instance %s to be in AutoScalingGroup with status %s",
					*instanceID, status)
			}

			sleepTime := 10 - (2 * retry)
			if sleepTime <= 0 {
				sleepTime = 1
			}
			time.Sleep(time.Duration(sleepTime) * time.Second)
		}
	}

	return errors.New("")
}

func (a *autoScalingGroup) findUnattachedInstanceLaunchedForThisASG() *instance {
	for inst := range a.region.instances.instances() {
		for _, tag := range inst.Tags {
			if *tag.Key == "launched-for-asg" && *tag.Value == a.name {
				if !a.hasMemberInstance(inst) {
					return inst
				}
			}
		}
	}
	return nil
}

func (a *autoScalingGroup) getAllowedInstanceTypes(baseInstance *instance) []string {
	var allowedInstanceTypesTag string

	// By default take the command line parameter
	allowed := strings.Replace(a.region.conf.AllowedInstanceTypes, " ", ",", -1)

	// Check option of allowed instance types
	// If we have that option we don't need to calculate the compatible instance type.
	if tagValue := a.getTagValue(AllowedInstanceTypesTag); tagValue != nil {
		allowedInstanceTypesTag = strings.Replace(*tagValue, " ", ",", -1)
	}

	// ASG Tag config has a priority to override
	if allowedInstanceTypesTag != "" {
		allowed = allowedInstanceTypesTag
	}

	if allowed == "current" {
		return []string{baseInstance.typeInfo.instanceType}
	}

	// Simple trick to avoid returning list with empty elements
	return strings.FieldsFunc(allowed, func(c rune) bool {
		return c == ','
	})
}

func (a *autoScalingGroup) getDisallowedInstanceTypes(baseInstance *instance) []string {
	var disallowedInstanceTypesTag string

	// By default take the command line parameter
	disallowed := strings.Replace(a.region.conf.DisallowedInstanceTypes, " ", ",", -1)

	// Check option of disallowed instance types
	// If we have that option we don't need to calculate the compatible instance type.
	if tagValue := a.getTagValue(DisallowedInstanceTypesTag); tagValue != nil {
		disallowedInstanceTypesTag = strings.Replace(*tagValue, " ", ",", -1)
	}

	// ASG Tag config has a priority to override
	if disallowedInstanceTypesTag != "" {
		disallowed = disallowedInstanceTypesTag
	}

	// Simple trick to avoid returning list with empty elements
	return strings.FieldsFunc(disallowed, func(c rune) bool {
		return c == ','
	})
}

func (a *autoScalingGroup) setAutoScalingMaxSize(maxSize int64) error {
	svc := a.region.services.autoScaling

	_, err := svc.UpdateAutoScalingGroup(
		&autoscaling.UpdateAutoScalingGroupInput{
			AutoScalingGroupName: aws.String(a.name),
			MaxSize:              aws.Int64(maxSize),
		})

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		log.Println(err.Error())
		return err
	}
	return nil
}

func (a *autoScalingGroup) attachSpotInstance(spotInstanceID string, wait bool) error {
	if wait {
		err := a.region.services.ec2.WaitUntilInstanceRunning(
			&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(spotInstanceID)},
			})

		if err != nil {
			log.Printf("Issue while waiting for instance %s to start: %v",
				spotInstanceID, err.Error())
		}

	}

	resp, err := a.region.services.autoScaling.AttachInstances(
		&autoscaling.AttachInstancesInput{
			AutoScalingGroupName: aws.String(a.name),
			InstanceIds: []*string{
				&spotInstanceID,
			},
		},
	)

	if err != nil {
		log.Println(err.Error())
		// Pretty-print the response data.
		log.Println(resp)
		return err
	}

	if err := a.waitForInstanceStatus(&spotInstanceID, "InService", 5); err != nil {
		log.Printf("Spot instance %s couldn't be attached to the group %s: %v",
			spotInstanceID, a.name, err.Error())
		return err
	}

	return nil
}

// Terminates an on-demand instance from the group,
// but only after it was detached from the autoscaling group
func (a *autoScalingGroup) detachAndTerminateOnDemandInstance(
	instanceID *string, wait bool) error {

	if wait {
		err := a.region.services.ec2.WaitUntilInstanceRunning(
			&ec2.DescribeInstancesInput{
				InstanceIds: []*string{instanceID},
			})

		if err != nil {
			log.Printf("Issue while waiting for instance %v to start: %v",
				instanceID, err.Error())
		}
	}

	log.Println(a.region.name,
		a.name,
		"Detaching and terminating instance:",
		*instanceID)
	// detach the on-demand instance
	detachParams := autoscaling.DetachInstancesInput{
		AutoScalingGroupName: aws.String(a.name),
		InstanceIds: []*string{
			instanceID,
		},
		ShouldDecrementDesiredCapacity: aws.Bool(true),
	}

	asSvc := a.region.services.autoScaling

	if _, err := asSvc.DetachInstances(&detachParams); err != nil {
		log.Println(err.Error())
		return err
	}

	// Wait till detachment initialize is complete before terminate instance
	time.Sleep(20 * time.Second * a.region.conf.SleepMultiplier)

	return a.region.instances.get(*instanceID).terminate()
}

// Terminates an instance from the group using the
// TerminateInstanceInAutoScalingGroup api call.
func (a *autoScalingGroup) terminateInstanceInAutoScalingGroup(
	instanceID *string, wait bool, decreaseCapacity bool) error {

	if wait {
		err := a.region.services.ec2.WaitUntilInstanceRunning(
			&ec2.DescribeInstancesInput{
				InstanceIds: []*string{instanceID},
			})

		if err != nil {
			log.Printf("Issue while waiting for instance %v to start: %v",
				instanceID, err.Error())
		}

		if err = a.waitForInstanceStatus(instanceID, "InService", 5); err != nil {
			log.Printf("Instance %s is still not InService, trying to terminate it anyway.",
				*instanceID)
		}
	}

	log.Println(a.region.name,
		a.name,
		"Terminating instance:",
		*instanceID)

	asSvc := a.region.services.autoScaling

	resDLH, err := asSvc.DescribeLifecycleHooks(
		&autoscaling.DescribeLifecycleHooksInput{
			AutoScalingGroupName: a.AutoScalingGroupName,
		})

	if err != nil {
		log.Println(err.Error())
		return err
	}

	for _, hook := range resDLH.LifecycleHooks {
		asSvc.CompleteLifecycleAction(
			&autoscaling.CompleteLifecycleActionInput{
				AutoScalingGroupName:  a.AutoScalingGroupName,
				InstanceId:            instanceID,
				LifecycleHookName:     hook.LifecycleHookName,
				LifecycleActionResult: aws.String("ABANDON"),
			})
	}

	resTIIASG, err := asSvc.TerminateInstanceInAutoScalingGroup(
		&autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     instanceID,
			ShouldDecrementDesiredCapacity: aws.Bool(decreaseCapacity),
		})

	if err != nil {
		log.Println(err.Error())
		return err
	}

	if resTIIASG != nil && resTIIASG.Activity != nil && resTIIASG.Activity.Description != nil {
		log.Println(*resTIIASG.Activity.Description)
	}

	return nil
}

// Counts the number of already running instances on-demand or spot, in any or a specific AZ.
func (a *autoScalingGroup) alreadyRunningInstanceCount(
	spot bool, availabilityZone *string) (int64, int64) {

	var total, count int64
	instanceCategory := Spot

	if !spot {
		instanceCategory = OnDemand
	}
	log.Println(a.name, "Counting already running", instanceCategory, "instances")
	for inst := range a.instances.instances() {

		if *inst.Instance.State.Name == "running" {
			// Count total running instances
			total++
			if availabilityZone == nil || *inst.Placement.AvailabilityZone == *availabilityZone {
				if (spot && inst.isSpot()) || (!spot && !inst.isSpot()) {
					count++
				}
			}
		}
	}
	log.Println(a.name, "Found", count, instanceCategory, "instances running on a total of", total)
	return count, total
}

func (a *autoScalingGroup) suspendProcesses() {
	AutoScalingProcessesToSuspend := []*string{aws.String("Terminate"), aws.String("AZRebalance")}
	log.Printf("Suspending processes on ASG %s", a.name)

	_, err := a.region.services.autoScaling.SuspendProcesses(
		&autoscaling.ScalingProcessQuery{
			AutoScalingGroupName: a.AutoScalingGroupName,
			ScalingProcesses:     AutoScalingProcessesToSuspend,
		})
	if err != nil {
		log.Printf("couldn't suspend processes on ASG %s ", a.name)
	}
}

func (a *autoScalingGroup) resumeProcesses() {
	AutoScalingProcessesToResume := []*string{aws.String("Terminate"), aws.String("AZRebalance")}
	log.Printf("Resuming processes on ASG %s", a.name)

	_, err := a.region.services.autoScaling.ResumeProcesses(
		&autoscaling.ScalingProcessQuery{
			AutoScalingGroupName: a.AutoScalingGroupName,
			ScalingProcesses:     AutoScalingProcessesToResume,
		})
	if err != nil {
		log.Printf("couldn't resume processes on ASG %s ", a.name)
	}
}
